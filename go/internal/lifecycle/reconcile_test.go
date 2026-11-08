package lifecycle

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"awgrotator/internal/fleet"
	"awgrotator/internal/node"
	"awgrotator/internal/proto"
	"awgrotator/internal/stores"
)

// ---- retiring waits for the places to agree

func TestRetireWaitsUntilEveryPlaceHasTheNewConfiguration(t *testing.T) {
	r := newRig(t)
	r.host.status = node.Status{UpdatedAt: r.clock.Now(),
		Peers: []node.PeerStatus{{LastHandshake: r.clock.Now()}}}
	res := movedAndPublished(t, r)
	published := r.clock.Now()

	// GitLab catches up a few looks into the wait.
	r.pub.notSynced = true
	r.pub.syncAfter = 3
	r.pub.reconciles = 0

	if err := r.eng.Retire(context.Background(), res.AttemptID); err != nil {
		t.Fatal(err)
	}
	if r.pub.reconciles != 4 {
		t.Errorf("looked %d times, want 4: three that found a place behind, one that found them level", r.pub.reconciles)
	}
	waited := r.clock.Now().Sub(published)
	if waited < r.cfg.Policy.Grace()+3*30*time.Second {
		t.Errorf("retired after %s: it did not wait for the place that was behind", waited)
	}
	if waited >= MaxLinger {
		t.Errorf("retired after %s, it should have gone as soon as the places agreed", waited)
	}
	assertOnlyDeleted(t, r, r.cfg.InstanceID)
}

func TestRetireGivesUpOnAPlaceThatNeverCatchesUpAfterAnHour(t *testing.T) {
	for name, set := range map[string]func(*rig){
		"a place behind":        func(r *rig) { r.pub.notSynced = true },
		"no place can be asked": func(r *rig) { r.pub.reconcileErr = errors.New("both unreachable") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.host.status = node.Status{UpdatedAt: r.clock.Now(),
				Peers: []node.PeerStatus{{LastHandshake: r.clock.Now()}}}
			res := movedAndPublished(t, r)
			published := r.clock.Now()
			set(r)

			if err := r.eng.Retire(context.Background(), res.AttemptID); err != nil {
				t.Fatal(err)
			}
			waited := r.clock.Now().Sub(published)
			if waited < MaxLinger || waited > MaxLinger+time.Minute {
				t.Errorf("retired after %s, want about %s", waited, MaxLinger)
			}
			assertOnlyDeleted(t, r, r.cfg.InstanceID)
		})
	}
}

func TestIdleRetirementAlsoWaitsForThePlacesToAgree(t *testing.T) {
	r := newRig(t)
	r.host.status = node.Status{UpdatedAt: r.clock.Now()} // nobody connected
	res := movedAndPublished(t, r)
	published := r.clock.Now()

	r.pub.notSynced = true
	r.pub.syncAfter = 2
	r.pub.reconciles = 0
	if err := r.eng.Retire(context.Background(), res.AttemptID); err != nil {
		t.Fatal(err)
	}
	if r.pub.reconciles != 3 {
		t.Errorf("%d looks, want 3", r.pub.reconciles)
	}
	if waited := r.clock.Now().Sub(published); waited >= r.cfg.Policy.Grace() {
		t.Errorf("retired after %s although nobody was connected", waited)
	}
}

// ---- the manager keeps the places in step

func TestTheManagerLooksAtThePlacesAtStartAndThenSoonerWhileOneIsBehind(t *testing.T) {
	r := newRig(t)
	m := started(t, r)

	// One place is behind from the start, as a fresh server may well find.
	r.pub.notSynced = true
	tick(t, m)
	if r.pub.reconciles != 1 {
		t.Fatalf("%d looks on the first pass, want 1", r.pub.reconciles)
	}

	// It is looked at again after five minutes, not before.
	r.clock.Advance(4 * time.Minute)
	tick(t, m)
	if r.pub.reconciles != 1 {
		t.Fatalf("looked again after 4 minutes (%d)", r.pub.reconciles)
	}
	r.clock.Advance(2 * time.Minute)
	tick(t, m)
	if r.pub.reconciles != 2 {
		t.Fatalf("%d looks after 6 minutes, want 2", r.pub.reconciles)
	}
	r.clock.Advance(6 * time.Minute)
	tick(t, m)
	if r.pub.reconciles != 3 {
		t.Fatalf("still behind, so it must keep trying every few minutes (%d)", r.pub.reconciles)
	}

	// Once they agree, the next look is an hour away.
	r.pub.notSynced = false
	r.clock.Advance(6 * time.Minute)
	tick(t, m)
	if r.pub.reconciles != 4 {
		t.Fatalf("%d", r.pub.reconciles)
	}
	r.clock.Advance(30 * time.Minute)
	tick(t, m)
	if r.pub.reconciles != 4 {
		t.Errorf("looked again after half an hour although they agree (%d)", r.pub.reconciles)
	}
	r.clock.Advance(31 * time.Minute)
	tick(t, m)
	if r.pub.reconciles != 5 {
		t.Errorf("%d looks after the hour, want 5", r.pub.reconciles)
	}
}

func TestAPlaceThatCannotBeReachedIsTriedAgainSoon(t *testing.T) {
	r := newRig(t)
	m := started(t, r)
	r.pub.reconcileErr = errors.New("nobody answers")

	tick(t, m)
	r.clock.Advance(6 * time.Minute)
	tick(t, m)
	if r.pub.reconciles != 2 {
		t.Errorf("%d looks, want 2: a failure must not wait out the hour", r.pub.reconciles)
	}
}

// ---- the publisher

// memStore is a place that lives in a map.
type memStore struct {
	name string

	mu       sync.Mutex
	files    map[string]string
	writeErr error
	readErr  error
}

func newMem(name string) *memStore { return &memStore{name: name, files: map[string]string{}} }

func (m *memStore) Name() string { return m.name }

func (m *memStore) Read(_ context.Context, names ...string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readErr != nil {
		return nil, m.readErr
	}
	out := map[string]string{}
	for _, n := range names {
		if v, ok := m.files[n]; ok {
			out[n] = v
		}
	}
	return out, nil
}

func (m *memStore) Write(_ context.Context, files map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.writeErr != nil {
		return m.writeErr
	}
	for k, v := range files {
		m.files[k] = v
	}
	return nil
}

type publisherRig struct {
	pub            *StorePublisher
	github, gitlab *memStore
	keys           testKeys
	cfg            *Config
}

func newPublisherRig(t *testing.T) *publisherRig {
	t.Helper()
	keys := newTestKeys(t)
	cfg := testConfig(t, keys)
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	github, gitlab := newMem("GitHub"), newMem("GitLab")
	set := &stores.Set{
		Stores: []stores.Store{github, gitlab},
		Trust:  cfg.Trust(),
		Now:    func() time.Time { return t0 },
	}
	return &publisherRig{
		pub:    &StorePublisher{Cfg: cfg, Stores: set, J: j, Now: func() time.Time { return t0 }},
		github: github, gitlab: gitlab, keys: keys, cfg: cfg,
	}
}

func publishableBody(t *testing.T) *fleet.ConfigBody {
	t.Helper()
	gen, err := fleet.NewGeneration(fleet.GenOptions{Split: true})
	if err != nil {
		t.Fatal(err)
	}
	return gen.Body("sto", "203.0.113.10", fleet.ClientNet{DNS: []string{"198.18.0.1"}, AllowedIPs: []string{"198.18.0.0/15"}}, t0)
}

func seqOf(t *testing.T, p *publisherRig, blob string) uint64 {
	t.Helper()
	trust := p.cfg.Trust()
	_, st, err := trust.OpenDoc(blob, proto.KindConfig, t0, proto.State{})
	if err != nil {
		t.Fatalf("a published configuration does not verify: %v", err)
	}
	return st.LastSeq
}

func TestAPublicationGoesToEveryPlaceAndOneIsEnough(t *testing.T) {
	p := newPublisherRig(t)
	ctx := context.Background()
	body := publishableBody(t)

	seq, err := p.pub.PublishConfig(ctx, body)
	if err != nil || seq != 1 {
		t.Fatalf("seq %d, err %v", seq, err)
	}
	if p.github.files[proto.FileConfig] == "" || p.github.files[proto.FileConfig] != p.gitlab.files[proto.FileConfig] {
		t.Fatal("both places should hold the same publication")
	}

	// GitLab is down for the next one: the publication still happens.
	p.gitlab.writeErr = errors.New("502")
	seq, err = p.pub.PublishConfig(ctx, body)
	if err != nil || seq != 2 {
		t.Fatalf("one place down must not stop a publication: seq %d, err %v", seq, err)
	}
	if got := seqOf(t, p, p.github.files[proto.FileConfig]); got != 2 {
		t.Errorf("GitHub holds %d", got)
	}
	if got := seqOf(t, p, p.gitlab.files[proto.FileConfig]); got != 1 {
		t.Errorf("GitLab should still hold the old one, holds %d", got)
	}

	// Nothing has caught up, and the publisher says so.
	st, err := p.pub.Reconcile(ctx)
	if err != nil || st.Synced {
		t.Fatalf("a place that cannot be written is not synced: %+v, %v", st, err)
	}

	// GitLab is back: the next look brings it level, without a new publication.
	p.gitlab.writeErr = nil
	st, err = p.pub.Reconcile(ctx)
	if err != nil || !st.Synced || !st.Wrote {
		t.Fatalf("%+v, %v", st, err)
	}
	if got := seqOf(t, p, p.gitlab.files[proto.FileConfig]); got != 2 {
		t.Errorf("GitLab caught up to %d, want 2", got)
	}
	if st, _ := p.pub.Reconcile(ctx); !st.Synced || st.Wrote {
		t.Errorf("level places were written to again: %+v", st)
	}

	// Neither takes it: the move must not be reported as published.
	p.github.writeErr = errors.New("403")
	p.gitlab.writeErr = errors.New("502")
	if _, err := p.pub.PublishConfig(ctx, body); err == nil {
		t.Error("a publication nobody took was reported as done")
	}
}

func TestNumberingContinuesFromWhicheverPlaceIsAhead(t *testing.T) {
	p := newPublisherRig(t)
	ctx := context.Background()
	body := publishableBody(t)

	// Publish a few times so that both hold a real signed document, then let
	// GitHub fall behind.
	for range 3 {
		if _, err := p.pub.PublishConfig(ctx, body); err != nil {
			t.Fatal(err)
		}
	}
	p.github.writeErr = errors.New("down")
	if _, err := p.pub.PublishConfig(ctx, body); err != nil { // seq 4, GitLab only
		t.Fatal(err)
	}
	p.github.writeErr = nil

	// A successor starts fresh: its own record says nothing, and the place it
	// would have asked first is the stale one.
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	successor := *p.pub
	successor.J = j
	seq, err := successor.PublishConfig(ctx, body)
	if err != nil || seq != 5 {
		t.Fatalf("seq %d, err %v: numbering must follow the newest place, or two different publications share a number", seq, err)
	}
}

func TestWhatIsPublishedAndWhatTheOperatorSaidAreReadFromTheFreshestPlace(t *testing.T) {
	p := newPublisherRig(t)
	ctx := context.Background()
	body := publishableBody(t)

	if _, err := p.pub.PublishConfig(ctx, body); err != nil { // 1, both
		t.Fatal(err)
	}
	p.github.writeErr = errors.New("down")
	other := *body
	other.Endpoint = "198.51.100.7:4000"
	if _, err := p.pub.PublishConfig(ctx, &other); err != nil { // 2, GitLab only
		t.Fatal(err)
	}

	cur, seq, err := p.pub.Current(ctx)
	if err != nil || seq != 2 || cur.Endpoint != other.Endpoint {
		t.Fatalf("Current: seq %d, endpoint %v, err %v", seq, cur, err)
	}

	// The operator's command reached only GitHub; the server still sees it.
	id, _ := proto.NewCommandID()
	blob, err := proto.SealCommand(&proto.Command{ID: id, IssuedAt: t0.Unix(), Action: proto.ActionMove},
		proto.IssuerRoot, p.keys.root, p.keys.content)
	if err != nil {
		t.Fatal(err)
	}
	p.github.files[proto.FileCommand] = blob
	in, err := p.pub.ReadInbox(ctx)
	if err != nil || in.Command != blob {
		t.Fatalf("the inbox missed a command that is in one place: %v", err)
	}

	// With every place down, nothing can be said.
	p.github.readErr, p.gitlab.readErr = errors.New("a"), errors.New("b")
	if _, _, err := p.pub.Current(ctx); err == nil || !strings.Contains(err.Error(), "GitHub") {
		t.Errorf("Current with nothing readable: %v", err)
	}
	if _, err := p.pub.ReadInbox(ctx); err == nil {
		t.Error("the inbox was read from nowhere")
	}
}

func TestAConfigurationMayNameGitLabAndMustBeComplete(t *testing.T) {
	cfg := testConfig(t, newTestKeys(t))
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.GitLab = &GitLabSync{Base: "https://gitlab.com", Project: 7, Package: "sync", Token: "glpat-x"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a complete block was refused: %v", err)
	}
	for name, mut := range map[string]func(*GitLabSync){
		"no token":     func(g *GitLabSync) { g.Token = "" },
		"no project":   func(g *GitLabSync) { g.Project = 0 },
		"no package":   func(g *GitLabSync) { g.Package = "" },
		"plain http":   func(g *GitLabSync) { g.Base = "http://gitlab.com" },
		"empty adress": func(g *GitLabSync) { g.Base = "" },
	} {
		c := testConfig(t, newTestKeys(t))
		c.GitLab = &GitLabSync{Base: "https://gitlab.com", Project: 7, Package: "sync", Token: "glpat-x"}
		mut(c.GitLab)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: an incomplete block was accepted", name)
		}
	}

	// And a configuration without one, as every server so far has, still loads.
	raw, err := testConfig(t, newTestKeys(t)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "gitlab") {
		t.Error("a configuration without a second place should not mention one")
	}
	if _, err := ParseConfig(raw); err != nil {
		t.Errorf("an older configuration no longer loads: %v", err)
	}
}
