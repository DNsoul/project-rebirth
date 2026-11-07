package soak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"awgrotator/internal/awg"
)

// ---------------------------------------------------------------- the config

func goodAWGArm(name string) Arm {
	return Arm{
		Name: name, Kind: KindAWG, ServerIP: "203.0.113.9", DirectTCP: []int{22},
		AWG: &AWGTarget{
			Endpoint: "203.0.113.9:41820", ClientAddr: "10.8.0.250",
			Params: awg.Params{Jc: 4, Jmin: 40, Jmax: 90, S1: 20, S2: 20, S3: 20, S4: 20},
		},
	}
}

func TestConfigDefaultsAreTheAgreedOnes(t *testing.T) {
	c := Config{Arms: []Arm{goodAWGArm("a")}}.WithDefaults()
	if c.ReconnectEvery.D() != 4*time.Hour {
		t.Errorf("reconnect every %s, want 4h as asked", c.ReconnectEvery.D())
	}
	if c.ProbeEvery.D() != 30*time.Second || c.DirectEvery.D() != time.Minute {
		t.Errorf("probe intervals: %s, %s", c.ProbeEvery.D(), c.DirectEvery.D())
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigReadsDurationsAsText(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"reconnect_every":"90m","arms":[]}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.ReconnectEvery.D() != 90*time.Minute {
		t.Errorf("got %s", c.ReconnectEvery.D())
	}
	if err := json.Unmarshal([]byte(`{"reconnect_every":5}`), &c); err == nil {
		t.Error("a bare number was accepted as a duration")
	}
}

func TestConfigRefusesWhatCannotRun(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"no arms":           func(c *Config) { c.Arms = nil },
		"a bad name":        func(c *Config) { c.Arms[0].Name = "Has Spaces" },
		"a duplicate name":  func(c *Config) { c.Arms = append(c.Arms, goodAWGArm("a")) },
		"no server address": func(c *Config) { c.Arms[0].ServerIP = "" },
		"an awg arm without a target": func(c *Config) {
			c.Arms[0].AWG = nil
		},
		"a socks arm without its restart command": func(c *Config) {
			c.Arms[0] = Arm{Name: "o", Kind: KindSOCKS, ServerIP: "203.0.113.9", SOCKS: "127.0.0.1:1080"}
		},
		"an unknown kind": func(c *Config) { c.Arms[0].Kind = "wireguard" },
		"reconnecting every few seconds": func(c *Config) {
			c.ReconnectEvery = Duration(5 * time.Second)
		},
	} {
		c := Config{Arms: []Arm{goodAWGArm("a")}}.WithDefaults()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadReadsAFileAndExplainsAMistake(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "soak.json")
	raw, _ := json.Marshal(Config{Arms: []Arm{goodAWGArm("awg-one")}})
	if err := os.WriteFile(good, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(good)
	if err != nil {
		t.Fatal(err)
	}
	if c.LogDir != "soak-logs" || c.Arms[0].Name != "awg-one" {
		t.Errorf("loaded %+v", c)
	}

	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"arms":[{"name":"x","kind":"awg"}]}`), 0o600)
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Errorf("error does not say which file: %v", err)
	}
}

// ---------------------------------------------------------------------- ping

func TestPingOutputInEnglishAndInRussian(t *testing.T) {
	english := "Pinging 1.1.1.1 with 32 bytes of data:\r\nReply from 1.1.1.1: bytes=32 time=63ms TTL=57\r\n"
	if rtt, err := parsePing(english, nil); err != nil || rtt != 63*time.Millisecond {
		t.Errorf("english: %v, %v", rtt, err)
	}
	// A Russian Windows prints in a legacy code page; only the ASCII survives.
	russian := "\xaf\xae\xa4\xe1\xe2\xa0\xa2\xa8\xa2\xa0 1.1.1.1: \xe7\xa8\xe1\xab\xae \xa1\xa0\xa9\xe2=32 \xa2\xe0\xa5\xac\xef=12\xac\xe1 TTL=57\r\n"
	if rtt, err := parsePing(russian, nil); err != nil || rtt != 12*time.Millisecond {
		t.Errorf("russian: %v, %v", rtt, err)
	}
	fast := "Reply from 192.168.1.1: bytes=32 time<1ms TTL=64"
	if _, err := parsePing(fast, nil); err != nil {
		t.Errorf("sub-millisecond reply: %v", err)
	}
}

func TestPingTimeoutIsAFailureWhateverTheLanguage(t *testing.T) {
	for _, out := range []string{
		"Request timed out.",
		"\x92\xa0\xa9\xac-\xa0\xe3\xe2 \xae\xa6\xa8\xa4\xa0\xad\xa8\xef \xa8\xe1\xe2\xa5\xab.",
		"Destination host unreachable.",
	} {
		if _, err := parsePing(out, errors.New("exit status 1")); err == nil {
			t.Errorf("%q counted as a reply", out)
		}
	}
	if _, err := parsePing("Request timed out.", nil); err == nil {
		t.Error("a timeout with a clean exit counted as a reply")
	}
}

// ------------------------------------------------------------------- traffic

func TestAssetsAreResolvedAgainstThePageAndDeduplicated(t *testing.T) {
	base, _ := url.Parse("https://example.org/news/")
	page := `<link href="/s/app.css"><script src="app.js"></script><script src="app.js"></script>` +
		`<img src="https://cdn.example.org/a.png"><a href="/about">x</a><img src="data:image/png;base64,AAA.png">`
	got := assetsOf(page, base, 10)
	want := []string{
		"https://example.org/s/app.css",
		"https://example.org/news/app.js",
		"https://cdn.example.org/a.png",
	}
	if len(got) != len(want) {
		t.Fatalf("assets = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("asset %d = %s, want %s", i, got[i], want[i])
		}
	}
	if n := len(assetsOf(page, base, 1)); n != 1 {
		t.Errorf("the cap was ignored: %d", n)
	}
}

func TestBehaviourMixHasQuietTimesAndStopsStreamingOverBudget(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	counts := map[behaviour]int{}
	for i := 0; i < 2000; i++ {
		counts[choose(r, false)]++
	}
	for _, b := range []behaviour{browse, stream, idle} {
		if counts[b] < 200 {
			t.Errorf("behaviour %d came up %d times in 2000", b, counts[b])
		}
	}
	for i := 0; i < 500; i++ {
		if choose(r, true) == stream {
			t.Fatal("it streamed while over budget")
		}
	}
}

func TestBudgetFollowsTheClock(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	a := &armRunner{cfg: Config{DailyGB: 4}, clock: func() time.Time { return now }, started: now}
	if a.overBudget() {
		t.Error("over budget at the very start")
	}
	a.bytesIn.Store(500e6) // more than the 200 MB head start allows at minute zero
	if !a.overBudget() {
		t.Error("500 MB in no time is not over budget")
	}
	now = now.Add(12 * time.Hour) // half a day entitles it to 2 GB
	a.bytesIn.Store(1_900e6)
	if a.overBudget() {
		t.Error("1.9 GB after half a day is within 4 GB a day")
	}
	a.bytesIn.Store(2_500e6)
	if !a.overBudget() {
		t.Error("2.5 GB after half a day is over 4 GB a day")
	}
}

// -------------------------------------------------------------------- runner

// fakeTunnel stands in for a tunnel.
type fakeTunnel struct {
	closed bool
}

func (f *fakeTunnel) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("not in tests")
}
func (f *fakeTunnel) Ping(context.Context, netip.Addr) (time.Duration, error) { return 0, ErrNoPing }
func (f *fakeTunnel) Close()                                                  { f.closed = true }

type testRig struct {
	a      *armRunner
	events *bytes.Buffer
	now    time.Time
	slept  []time.Duration
	mu     sync.Mutex
}

func newRig(t *testing.T, arm Arm) *testRig {
	t.Helper()
	dir := t.TempDir()
	console := &bytes.Buffer{}
	log, err := newEventLog(dir, console)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(log.close)
	rig := &testRig{events: console, now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	a := &armRunner{arm: arm, cfg: Config{}.WithDefaults(), log: log}
	a.clock = func() time.Time { rig.mu.Lock(); defer rig.mu.Unlock(); return rig.now }
	a.sleepFn = func(ctx context.Context, d time.Duration) error {
		rig.mu.Lock()
		rig.now = rig.now.Add(d)
		rig.slept = append(rig.slept, d)
		rig.mu.Unlock()
		return ctx.Err()
	}
	rig.a = a
	return rig
}

func TestBringUpKeepsTryingAndSaysWhatHappenedEachTime(t *testing.T) {
	rig := newRig(t, goodAWGArm("awg-x"))
	attempts := 0
	rig.a.connect = func(context.Context) (Tunnel, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("no reply from the server")
		}
		return &fakeTunnel{}, nil
	}
	rig.a.probe = func(context.Context, Tunnel) bool { return true }

	tun, ok := rig.a.bringUp(context.Background(), false)
	if !ok || tun == nil {
		t.Fatal("it gave up")
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if len(rig.slept) != 2 || rig.slept[0] != retryAfter {
		t.Errorf("pauses between attempts = %v", rig.slept)
	}
	out := rig.events.String()
	for _, want := range []string{"connect FAILED: no reply from the server", "reconnect ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "connect FAILED") != 2 {
		t.Errorf("expected two failures in the log:\n%s", out)
	}
}

func TestFirstConnectionIsUpAndLaterOnesAreReconnections(t *testing.T) {
	rig := newRig(t, goodAWGArm("awg-x"))
	rig.a.connect = func(context.Context) (Tunnel, error) { return &fakeTunnel{}, nil }
	rig.a.probe = func(context.Context, Tunnel) bool { return true }
	if _, ok := rig.a.bringUp(context.Background(), true); !ok {
		t.Fatal("first")
	}
	if !strings.Contains(rig.events.String(), " up ok") {
		t.Errorf("the first connection is not logged as up:\n%s", rig.events.String())
	}
}

func TestATunnelThatConnectsButCarriesNothingIsNotUp(t *testing.T) {
	rig := newRig(t, goodAWGArm("awg-x"))
	var made []*fakeTunnel
	rig.a.connect = func(context.Context) (Tunnel, error) {
		f := &fakeTunnel{}
		made = append(made, f)
		return f, nil
	}
	calls := 0
	rig.a.probe = func(context.Context, Tunnel) bool {
		calls++
		return len(made) >= 2 && calls > 40 // the second tunnel works, the first never does
	}
	tun, ok := rig.a.bringUp(context.Background(), false)
	if !ok || tun == nil {
		t.Fatal("it gave up")
	}
	if len(made) < 2 || !made[0].closed {
		t.Errorf("the dead tunnel was not closed and replaced: made %d, first closed %v", len(made), made[0].closed)
	}
}

func TestSocksTunnelsAreRestartedOnReconnectionButNotAtTheStart(t *testing.T) {
	arm := Arm{Name: "ovpn-x", Kind: KindSOCKS, ServerIP: "203.0.113.9", SOCKS: "127.0.0.1:1080", Restart: []string{"restart"}}
	rig := newRig(t, arm)
	restarts := 0
	rig.a.restart = func(context.Context) error { restarts++; return nil }
	rig.a.connect = func(context.Context) (Tunnel, error) { return &fakeTunnel{}, nil }
	rig.a.probe = func(context.Context, Tunnel) bool { return true }

	_, _ = rig.a.bringUp(context.Background(), true)
	if restarts != 0 {
		t.Errorf("the tunnel was restarted at the start (%d)", restarts)
	}
	_, _ = rig.a.bringUp(context.Background(), false)
	if restarts != 1 {
		t.Errorf("restarts after a reconnection = %d, want 1", restarts)
	}
}

func TestAFailedRestartCountsAsAFailedAttempt(t *testing.T) {
	arm := Arm{Name: "ovpn-x", Kind: KindSOCKS, ServerIP: "203.0.113.9", SOCKS: "127.0.0.1:1080", Restart: []string{"restart"}}
	rig := newRig(t, arm)
	n := 0
	rig.a.restart = func(context.Context) error {
		n++
		if n == 1 {
			return errors.New("exit status 1")
		}
		return nil
	}
	rig.a.connect = func(context.Context) (Tunnel, error) { return &fakeTunnel{}, nil }
	rig.a.probe = func(context.Context, Tunnel) bool { return true }
	if _, ok := rig.a.bringUp(context.Background(), false); !ok {
		t.Fatal("it gave up")
	}
	if !strings.Contains(rig.events.String(), "restart: exit status 1") {
		t.Errorf("the failed restart is not in the log:\n%s", rig.events.String())
	}
}

func TestProbeLoopSaysWhenATunnelThatIsUpStopsAnsweringAndWhenItRecovers(t *testing.T) {
	rig := newRig(t, goodAWGArm("awg-x"))
	results := []bool{true, false, false, false, false, false, true, true}
	i := 0
	ctx, cancel := context.WithCancel(context.Background())
	rig.a.probe = func(context.Context, Tunnel) bool {
		r := results[i]
		i++
		if i == len(results) {
			cancel()
		}
		return r
	}
	rig.a.probeLoop(ctx, &fakeTunnel{})
	out := rig.events.String()
	if strings.Count(out, "stalled") != 1 {
		t.Errorf("stalled should be reported once:\n%s", out)
	}
	if !strings.Contains(out, "recovered") {
		t.Errorf("recovery is not reported:\n%s", out)
	}
	if strings.Index(out, "stalled") > strings.Index(out, "recovered") {
		t.Errorf("recovered before stalled:\n%s", out)
	}
}

func TestAFewLostProbesAreNotAStall(t *testing.T) {
	rig := newRig(t, goodAWGArm("awg-x"))
	results := []bool{false, false, true, false, false, true, true}
	i := 0
	ctx, cancel := context.WithCancel(context.Background())
	rig.a.probe = func(context.Context, Tunnel) bool {
		r := results[i]
		i++
		if i == len(results) {
			cancel()
		}
		return r
	}
	rig.a.probeLoop(ctx, &fakeTunnel{})
	if strings.Contains(rig.events.String(), "stalled") {
		t.Errorf("a stall was reported for scattered losses:\n%s", rig.events.String())
	}
}

// ----------------------------------------------------------------- the report

func writeEvents(t *testing.T, dir, arm string, events []Event) {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range events {
		e.Arm = arm
		line, _ := json.Marshal(e)
		buf.Write(append(line, '\n'))
	}
	if err := os.WriteFile(filepath.Join(dir, arm+".jsonl"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReportFindsWhenTheServerStoppedAnsweringBeforeTheTunnelBroke(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var ev []Event
	ev = append(ev, Event{At: t0, Ev: "start", Data: map[string]any{"kind": "awg"}})
	// Ten minutes of direct probes: good for five, then the server goes quiet,
	// while the tunnel keeps answering.
	for i := 0; i < 10; i++ {
		at := t0.Add(time.Duration(i) * time.Minute)
		ev = append(ev,
			Event{At: at, Ev: "direct", Kind: "tcp/22", OK: boolp(i < 5)},
			Event{At: at, Ev: "direct", Kind: "icmp", OK: boolp(i < 5)},
			Event{At: at, Ev: "probe", Kind: "http", OK: boolp(true)},
		)
	}
	writeEvents(t, dir, "awg-x", ev)

	reports, err := Summarize(dir)
	if err != nil || len(reports) != 1 {
		t.Fatalf("summarize: %v %v", reports, err)
	}
	v := reports[0].Verdict()
	if len(v) != 1 || !strings.Contains(v[0], "напрямую") || !strings.Contains(v[0], "5 мин") {
		t.Errorf("verdict = %v", v)
	}
	if p := reports[0].Inside["http"]; p == nil || p.Failed != 0 {
		t.Errorf("the tunnel was reported as failing: %+v", p)
	}
}

func TestReportDoesNotMistakeAnIsolatedLossForADeath(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	ev := []Event{{At: t0, Ev: "start", Data: map[string]any{"kind": "awg"}}}
	for i := 0; i < 20; i++ {
		ev = append(ev, Event{At: t0.Add(time.Duration(i) * time.Minute), Ev: "direct", Kind: "icmp", OK: boolp(i%4 != 0)})
	}
	writeEvents(t, dir, "awg-x", ev)
	reports, _ := Summarize(dir)
	if v := reports[0].Verdict(); len(v) != 1 || v[0] != "сбоев нет" {
		t.Errorf("verdict = %v", v)
	}
}

func TestReportCountsReconnectionsAndFailedConnections(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	ev := []Event{
		{At: t0, Ev: "start", Data: map[string]any{"kind": "socks"}},
		{At: t0, Ev: "up", OK: boolp(true), Data: map[string]any{"seconds": 3.0, "attempt": 1.0}},
		{At: t0.Add(4 * time.Hour), Ev: "connect", OK: boolp(false), Err: "nothing came through", Data: map[string]any{"attempt": 1.0}},
		{At: t0.Add(4*time.Hour + time.Minute), Ev: "connect", OK: boolp(false), Err: "nothing came through", Data: map[string]any{"attempt": 2.0}},
		{At: t0.Add(4*time.Hour + 5*time.Minute), Ev: "stalled", OK: boolp(false)},
		{At: t0.Add(5 * time.Hour), Ev: "volume", Data: map[string]any{"in": 2e9, "out": 1e8, "requests_ok": 100.0, "requests_failed": 3.0}},
	}
	writeEvents(t, dir, "ovpn-x", ev)
	reports, _ := Summarize(dir)
	r := reports[0]
	if len(r.Reconnections) != 3 || len(r.Stalls) != 1 {
		t.Fatalf("reconnections %d, stalls %d", len(r.Reconnections), len(r.Stalls))
	}
	if r.InBytes != 2e9 || r.FetchFailed != 3 {
		t.Errorf("volume: %d in, %d failed", r.InBytes, r.FetchFailed)
	}
	if v := r.Verdict(); !strings.Contains(strings.Join(v, "\n"), "неудачное подключение") {
		t.Errorf("verdict = %v", v)
	}
	text := Render(reports)
	for _, want := range []string{"ovpn-x", "НЕ УДАЛОСЬ", "2.10 ГБ", "зависаний туннеля 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report lacks %q:\n%s", want, text)
		}
	}
}

func TestReportSurvivesAHalfWrittenLastLine(t *testing.T) {
	dir := t.TempDir()
	good, _ := json.Marshal(Event{At: time.Now(), Arm: "a", Ev: "start", Data: map[string]any{"kind": "awg"}})
	_ = os.WriteFile(filepath.Join(dir, "a.jsonl"), append(append(good, '\n'), []byte(`{"t":"2026-11-0`)...), 0o644)
	if _, err := Summarize(dir); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- the log

func TestAQuietEventGoesToTheFileOnly(t *testing.T) {
	dir := t.TempDir()
	console := &bytes.Buffer{}
	log, err := newEventLog(dir, console)
	if err != nil {
		t.Fatal(err)
	}
	log.emit(Event{At: time.Now(), Arm: "a", Ev: "probe", Kind: "http", OK: boolp(true)}, true)
	log.emit(Event{At: time.Now(), Arm: "a", Ev: "probe", Kind: "http", OK: boolp(false), Err: "timeout"}, false)
	log.close()

	if strings.Contains(console.String(), "ok") || !strings.Contains(console.String(), "FAILED: timeout") {
		t.Errorf("console:\n%s", console.String())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "a.jsonl"))
	if strings.Count(string(raw), "\n") != 2 {
		t.Errorf("the file should hold both events:\n%s", raw)
	}
}
