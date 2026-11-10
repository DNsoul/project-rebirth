package lifecycle

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"awgrotator/internal/fleet"
)

// publishedHere makes the places say this server is the one the apps use.
func (r *rig) publishedHere() {
	r.pub.current = &fleet.ConfigBody{Endpoint: fmt.Sprintf("%s:51820", r.cfg.IP), Region: r.cfg.Region}
}

func collect(m *Manager) *[]Event {
	var got []Event
	m.OnEvent = func(e Event) { got = append(got, e) }
	return &got
}

func apply(t *testing.T, m *Manager, a Action) Result {
	t.Helper()
	var res *Result
	a.Done = func(r Result) { res = &r }
	m.Apply(context.Background(), a)
	if res == nil {
		t.Fatal("the action was never reported done")
	}
	return *res
}

func TestTheServerTheAppsUseKnowsItAndSaysSoOnce(t *testing.T) {
	r := newRig(t)
	r.publishedHere()
	m := r.manager()
	events := collect(m)
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if s := m.Snapshot(); s.CurrentKnown {
		t.Fatal("known before anything was looked at")
	}
	if err := m.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if s := m.Snapshot(); !s.Current || !s.CurrentKnown || s.Region != r.cfg.Region {
		t.Fatalf("snapshot = %+v", s)
	}
	_ = m.Tick(ctx)
	n := 0
	for _, e := range *events {
		if e.Kind == EventCurrent {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("told %d times that this server took over", n)
	}
}

func TestAServerThatIsNotPublishedDoesNotAnswer(t *testing.T) {
	r := newRig(t)
	r.pub.current = &fleet.ConfigBody{Endpoint: "198.51.100.200:51820"}
	m := r.manager()
	ctx := context.Background()
	_ = m.Start(ctx)
	_ = m.Tick(ctx)
	if s := m.Snapshot(); s.Current || !s.CurrentKnown {
		t.Fatalf("snapshot = %+v", s)
	}
	if res := apply(t, m, Action{Kind: ActMove, ID: "x"}); res.Err == nil {
		t.Fatal("a server that is not the published one accepted a move")
	}
}

func TestAMoveThroughTheBotHappensOnceAndIsToldAbout(t *testing.T) {
	r := newRig(t)
	r.publishedHere()
	m := r.manager()
	events := collect(m)
	ctx := context.Background()
	_ = m.Start(ctx)
	_ = m.Tick(ctx)

	if res := apply(t, m, Action{Kind: ActMove, ID: "n1", Region: "waw"}); res.Err != nil || res.Skipped {
		t.Fatalf("move: %+v", res)
	}
	if res := apply(t, m, Action{Kind: ActMove, ID: "n1", Region: "waw"}); !res.Skipped {
		t.Fatalf("the same button twice should be skipped: %+v", res)
	}
	if err := m.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(r.prov.reqs) != 1 || r.prov.reqs[0].Region != "waw" {
		t.Fatalf("provisioned %+v", r.prov.reqs)
	}
	var kinds []EventKind
	for _, e := range *events {
		kinds = append(kinds, e.Kind)
	}
	want := []EventKind{EventCurrent, EventMoveStarted, EventMovePublished}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	if s := m.Snapshot(); s.Current || s.Moving {
		t.Fatalf("after handing over: %+v", s)
	}
	// The record of the button goes to the replacement with the journal.
	if !m.Eng.inheritedJournal().WasExecuted("bot:n1") {
		t.Fatal("the replacement would not know this button was already used")
	}
}

func TestAFailedMoveIsToldAbout(t *testing.T) {
	r := newRig(t)
	r.publishedHere()
	r.tester.report = failingReport()
	m := r.manager()
	events := collect(m)
	ctx := context.Background()
	_ = m.Start(ctx)
	_ = m.Tick(ctx)
	apply(t, m, Action{Kind: ActMove, ID: "n2"})
	_ = m.Tick(ctx)
	last := (*events)[len(*events)-1]
	if last.Kind != EventMoveFailed || last.Err == "" {
		t.Fatalf("last event = %+v", last)
	}
	if s := m.Snapshot(); !s.Current || s.Moving {
		t.Fatalf("after a failed move the server answers on: %+v", s)
	}
}

func TestARegionOutsideTheLineIsRefused(t *testing.T) {
	r := newRig(t)
	r.publishedHere()
	m := r.manager()
	_ = m.Start(context.Background())
	_ = m.Tick(context.Background())
	if res := apply(t, m, Action{Kind: ActMove, ID: "n3", Region: "mars"}); res.Err == nil {
		t.Fatal("moved to a region that is not on the list")
	}
}

func TestAutomaticMovesAreSwitchedAndSaved(t *testing.T) {
	r := newRig(t)
	m := r.manager()
	m.ConfigPath = filepath.Join(t.TempDir(), "manager.json")
	off, on := false, true
	if res := apply(t, m, Action{Kind: ActPolicy, AutoAge: &off, AutoBlock: &on}); res.Err != nil {
		t.Fatal(res.Err)
	}
	if r.cfg.Policy.AutoAge || !r.cfg.Policy.AutoBlock {
		t.Fatalf("policy = %+v", r.cfg.Policy)
	}
	saved, err := LoadConfig(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Policy.AutoAge || !saved.Policy.AutoBlock {
		t.Fatalf("saved policy = %+v", saved.Policy)
	}
	if s := m.Snapshot(); s.AutoAge || !s.AutoBlock {
		t.Fatalf("the snapshot shows the old policy: %+v", s)
	}
}

func TestACheckAskedForIsRunAndReported(t *testing.T) {
	r := newRig(t)
	m := r.manager()
	res := apply(t, m, Action{Kind: ActCheck})
	if res.Err != nil || res.Check == nil || res.Check.Round != Good || res.Check.Target.Total != 10 {
		t.Fatalf("check = %+v", res)
	}
	if r.prober.calls != 1 {
		t.Fatalf("prober called %d times", r.prober.calls)
	}
}

func TestTheTunnelIsRestartedWhenAsked(t *testing.T) {
	r := newRig(t)
	m := r.manager()
	if res := apply(t, m, Action{Kind: ActRestartNode, ID: "r1"}); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := apply(t, m, Action{Kind: ActRestartNode, ID: "r1"}); !res.Skipped {
		t.Fatal("restarted twice for one button")
	}
	if r.host.restarts != 1 {
		t.Fatalf("restarts = %d", r.host.restarts)
	}
}

func TestActionsQueueUpToALimit(t *testing.T) {
	r := newRig(t)
	m := r.manager()
	var err error
	for i := 0; i < 20 && err == nil; i++ {
		err = m.Do(Action{Kind: ActCheck})
	}
	if err != ErrBusy {
		t.Fatalf("err = %v, want ErrBusy once the queue is full", err)
	}
}
