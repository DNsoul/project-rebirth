package egress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"awgrotator/internal/warp"
)

type fakeSys struct {
	log       []string
	upErr     error
	probeErr  error
	routeErr  error
	routedNow bool
	upNow     bool
}

func (f *fakeSys) Up(_ context.Context, a *warp.Account) error {
	f.log = append(f.log, "up")
	if f.upErr != nil {
		return f.upErr
	}
	f.upNow = true
	return nil
}

func (f *fakeSys) Down(context.Context) error {
	f.log = append(f.log, "down")
	f.upNow, f.routedNow = false, false
	return nil
}

func (f *fakeSys) Route(_ context.Context, on bool) error {
	if on {
		f.log = append(f.log, "route on")
	} else {
		f.log = append(f.log, "route off")
	}
	if f.routeErr != nil {
		return f.routeErr
	}
	f.routedNow = on
	return nil
}

func (f *fakeSys) Probe(context.Context) error {
	f.log = append(f.log, "probe")
	return f.probeErr
}

func (f *fakeSys) Handshake() (time.Time, bool) { return time.Time{}, false }

type rig struct {
	sys   *fakeSys
	ctl   *Controller
	now   time.Time
	asked int
}

func newRig() *rig {
	r := &rig{sys: &fakeSys{}, now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	r.ctl = &Controller{
		Sys: r.sys,
		Account: func(context.Context) (*warp.Account, error) {
			r.asked++
			return &warp.Account{}, nil
		},
		Now: func() time.Time { return r.now },
	}
	return r
}

// step runs the controller as the node does: every ten seconds.
func (r *rig) step(want Mode, times int) {
	for i := 0; i < times; i++ {
		r.ctl.Step(context.Background(), want)
		r.now = r.now.Add(10 * time.Second)
	}
}

func TestNothingAskedNothingDone(t *testing.T) {
	r := newRig()
	r.step(Direct, 3)
	if len(r.sys.log) != 0 || r.asked != 0 {
		t.Errorf("did %v and asked for an account %d times", r.sys.log, r.asked)
	}
	if st := r.ctl.Status(); st.State != StateDirect || st.Want != Direct {
		t.Errorf("status %+v", st)
	}
}

func TestAskedForWARPTheClientsGoThroughItAsSoonAsItAnswers(t *testing.T) {
	r := newRig()
	r.step(WARP, 1)
	if got := strings.Join(r.sys.log, ", "); got != "up, probe, route on" {
		t.Errorf("did %q", got)
	}
	if st := r.ctl.Status(); st.State != StateWARP || st.Want != WARP {
		t.Errorf("status %+v", st)
	}
	r.sys.log = nil
	r.step(WARP, 6) // a minute
	for _, l := range r.sys.log {
		if l != "probe" {
			t.Fatalf("while all is well only probes are made, got %v", r.sys.log)
		}
	}
	if n := len(r.sys.log); n < 2 || n > 4 {
		t.Errorf("%d probes in a minute; every 20 s was meant", n)
	}
}

func TestWARPThatStopsAnsweringIsLeftAfterTwoFailedProbesAndTakenBackOnlyOnceItKeepsWorking(t *testing.T) {
	r := newRig()
	r.step(WARP, 1)
	r.sys.probeErr = errors.New("timeout")
	r.step(WARP, 2) // first failed probe
	if !r.sys.routedNow {
		t.Fatal("one failed probe moved the clients")
	}
	r.step(WARP, 2) // second
	if r.sys.routedNow {
		t.Fatal("still through WARP after two failed probes")
	}
	st := r.ctl.Status()
	if st.State != StateFallback || st.Error != "timeout" {
		t.Errorf("status %+v", st)
	}

	// WARP answers again: not taken back at once.
	r.sys.probeErr = nil
	r.step(WARP, 6) // a minute
	if r.sys.routedNow {
		t.Fatal("taken back after a minute of WARP working; two were meant")
	}
	r.step(WARP, 9)
	if !r.sys.routedNow || r.ctl.Status().State != StateWARP {
		t.Errorf("not taken back after WARP kept working: %+v", r.ctl.Status())
	}
}

func TestSwitchingOffNeedsNothingFromWARP(t *testing.T) {
	r := newRig()
	r.step(WARP, 1)
	r.sys.probeErr = errors.New("WARP is gone")
	r.sys.upErr = errors.New("cannot even come up")
	r.sys.log = nil

	r.step(Direct, 1)
	if got := strings.Join(r.sys.log, ", "); got != "route off, down" {
		t.Errorf("did %q", got)
	}
	if r.sys.routedNow || r.ctl.Status().State != StateDirect {
		t.Errorf("not direct: %+v", r.ctl.Status())
	}
}

func TestARegistrationThatFailsIsTriedAgainLaterNotAtEveryStep(t *testing.T) {
	r := newRig()
	r.ctl.Account = func(context.Context) (*warp.Account, error) {
		r.asked++
		return nil, errors.New("429 Too Many Requests")
	}
	r.step(WARP, 3) // 30 s
	if r.asked != 1 {
		t.Errorf("asked %d times in 30 s", r.asked)
	}
	st := r.ctl.Status()
	if st.State != StateFallback || !strings.Contains(st.Error, "429") {
		t.Errorf("status %+v", st)
	}
	if r.sys.routedNow {
		t.Error("clients routed into nothing")
	}
	r.step(WARP, 1)
	if r.asked != 2 {
		t.Errorf("not tried again after 30 s (asked %d)", r.asked)
	}
}

func TestAWARPThatNeverAnsweredIsShownAsStartingThenAsFallback(t *testing.T) {
	r := newRig()
	r.sys.probeErr = errors.New("no handshake")
	r.step(WARP, 1)
	if s := r.ctl.Status().State; s != StateStarting {
		t.Errorf("after the first failed probe: %s", s)
	}
	r.step(WARP, 3)
	if s := r.ctl.Status().State; s != StateFallback {
		t.Errorf("after the second: %s", s)
	}
	if r.sys.routedNow {
		t.Error("clients routed into a WARP that never answered")
	}
}

func TestTheWantFileDefaultsToDirect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exit.json")
	if m, err := ReadWant(path); m != Direct || err != nil {
		t.Errorf("missing file: %v, %v", m, err)
	}
	if err := WriteWant(path, WARP); err != nil {
		t.Fatal(err)
	}
	if m, err := ReadWant(path); m != WARP || err != nil {
		t.Errorf("after writing warp: %v, %v", m, err)
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, err := ReadWant(path); m != Direct || err == nil {
		t.Errorf("garbage: %v, %v", m, err)
	}
	if _, err := ParseMode("tor"); err == nil {
		t.Error("an unknown mode was taken")
	}
}

func TestAnAccountIsRegisteredOnceAndKept(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"id":"d1","token":"t","config":{"peers":[{"public_key":"bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=","endpoint":{"v4":"162.159.192.1:0","host":"engage.cloudflareclient.com:2408"}}],"interface":{"addresses":{"v4":"172.16.0.2"}}}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "warp.json")
	get := Accounts(path, &warp.Client{API: srv.URL}, nil)

	a, err := get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || a.PrivateKey != b.PrivateKey {
		t.Errorf("registered %d times; keys equal: %v", calls.Load(), a.PrivateKey == b.PrivateKey)
	}
}
