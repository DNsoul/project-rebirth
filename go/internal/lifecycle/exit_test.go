package lifecycle

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"awgrotator/internal/egress"
)

func TestTheNodeIsToldTheWayOutOnceAtTheFirstPassAndAgainOnlyWhenItChanges(t *testing.T) {
	r := newRig(t)
	m := r.manager()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := m.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(r.host.exitWrites, []egress.Mode{egress.Direct}) {
		t.Fatalf("the node was told %v; once \"direct\" was meant", r.host.exitWrites)
	}

	m.ConfigPath = filepath.Join(t.TempDir(), "manager.json")
	warp := egress.WARP
	if res := apply(t, m, Action{Kind: ActPolicy, Exit: &warp}); res.Err != nil {
		t.Fatal(res.Err)
	}
	if !slices.Equal(r.host.exitWrites, []egress.Mode{egress.Direct, egress.WARP}) {
		t.Fatalf("after switching on, the node was told %v; warp at once was meant", r.host.exitWrites)
	}
	saved, err := LoadConfig(m.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Policy.ExitMode() != egress.WARP {
		t.Errorf("the saved policy says %q: a restart or a replacement would forget it", saved.Policy.Exit)
	}
	if s := m.Snapshot(); s.ExitWant != egress.WARP {
		t.Errorf("the snapshot wants %q", s.ExitWant)
	}
}

func TestAReplacementInheritsTheWayOut(t *testing.T) {
	r := newRig(t)
	r.cfg.Policy.Exit = "warp"
	raw, err := r.cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	next, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if next.Policy.ExitMode() != egress.WARP {
		t.Errorf("a replacement's settings say %q", next.Policy.Exit)
	}
}

func TestAnUnknownWayOutIsRefused(t *testing.T) {
	p := DefaultPolicy()
	p.Exit = "tor"
	if err := p.Normalize(); err == nil {
		t.Error("taken")
	}
}

func TestTheOperatorHearsWhenTheWayOutChangesButNotAboutTheStepsBetween(t *testing.T) {
	r := newRig(t)
	m := r.manager()
	events := collect(m)
	ctx := context.Background()
	pass := func(state egress.State, errText string) {
		r.host.status.Exit = &egress.Status{Want: egress.WARP, State: state, Error: errText}
		m.publishSnapshot(ctx)
	}

	pass(egress.StateDirect, "")    // how it was found: not news
	pass(egress.StateStarting, "")  // a step on the way: not news
	pass(egress.StateWARP, "")      // on
	pass(egress.StateWARP, "")      // no change
	pass(egress.StateFallback, "x") // fell back
	pass(egress.StateStarting, "")  // ignored
	pass(egress.StateWARP, "")      // back

	var got []egress.State
	for _, e := range *events {
		if e.Kind == EventExit {
			got = append(got, e.Exit)
		}
	}
	want := []egress.State{egress.StateWARP, egress.StateFallback, egress.StateWARP}
	if !slices.Equal(got, want) {
		t.Errorf("told %v, want %v", got, want)
	}
	if s := m.Snapshot(); s.Exit == nil || s.Exit.State != egress.StateWARP {
		t.Errorf("snapshot exit = %+v", s.Exit)
	}
}
