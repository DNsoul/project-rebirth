package lifecycle

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"awgrotator/internal/deploy"
	"awgrotator/internal/fleet"
	"awgrotator/internal/node"
	"awgrotator/internal/proto"
	"awgrotator/internal/splitplan"
	"awgrotator/internal/tunneltest"
	"awgrotator/internal/vultr"
)

// ---- a clock that moves only when told to

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Sleep advances the clock instead of waiting, so a fifteen minute grace
// period costs nothing.
func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.Advance(d)
	return nil
}

// ---- a log of everything that happens to the outside world

type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(format string, args ...any) {
	e.mu.Lock()
	e.list = append(e.list, fmt.Sprintf(format, args...))
	e.mu.Unlock()
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.list...)
}

func (e *events) count(prefix string) int {
	n := 0
	for _, s := range e.all() {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// ---- fakes

type fakeCloud struct {
	mu        sync.Mutex
	ev        *events
	instances []vultr.Instance
	deleted   []string
	deleteErr error
	listErr   error
}

func (c *fakeCloud) Tagged(context.Context) ([]vultr.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ev.add("tagged")
	if c.listErr != nil {
		return nil, c.listErr
	}
	return append([]vultr.Instance(nil), c.instances...), nil
}

func (c *fakeCloud) Delete(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleteErr != nil {
		return c.deleteErr
	}
	c.ev.add("delete:%s", id)
	c.deleted = append(c.deleted, id)
	kept := c.instances[:0]
	for _, i := range c.instances {
		if i.ID != id {
			kept = append(kept, i)
		}
	}
	c.instances = kept
	return nil
}

func (c *fakeCloud) add(i vultr.Instance) {
	c.mu.Lock()
	c.instances = append(c.instances, i)
	c.mu.Unlock()
}

func (c *fakeCloud) has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, i := range c.instances {
		if i.ID == id {
			return true
		}
	}
	return false
}

func (c *fakeCloud) deletedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.deleted...)
}

type fakeProv struct {
	ev    *events
	cloud *fakeCloud
	err   error
	next  int

	reqs  []ProvisionRequest
	files []deploy.File // what FilesFor produced for the last request

	// dieAfterCreate simulates the process dying once the instance exists but
	// before the build finishes.
	dieAfterCreate error
}

func (p *fakeProv) Provision(_ context.Context, req ProvisionRequest) (*deploy.Result, error) {
	p.ev.add("provision")
	p.reqs = append(p.reqs, req)
	if p.err != nil {
		return nil, p.err
	}
	p.next++
	inst := vultr.Instance{
		ID:     fmt.Sprintf("new-%d", p.next),
		MainIP: fmt.Sprintf("198.51.100.%d", p.next),
		Region: req.Region,
		Tags:   []string{"awg-rotator"},
	}
	// The instance exists from here, as it does in the real provisioner.
	p.cloud.add(inst)
	if req.OnCreated != nil {
		req.OnCreated(&inst)
	}
	if p.dieAfterCreate != nil {
		return nil, p.dieAfterCreate
	}
	if req.FilesFor != nil {
		files, err := req.FilesFor(&inst)
		if err != nil {
			return nil, err
		}
		p.files = files
	}
	return &deploy.Result{InstanceID: inst.ID, IP: inst.MainIP, Region: req.Region}, nil
}

type fakeTester struct {
	ev     *events
	report tunneltest.Report
	err    error
	target tunneltest.Target
}

func passingReport() tunneltest.Report {
	return tunneltest.Report{Checks: []tunneltest.Check{{Name: "all", OK: true, Required: true}}}
}

func failingReport() tunneltest.Report {
	return tunneltest.Report{Checks: []tunneltest.Check{{Name: "site", Required: true, Detail: "timed out"}}}
}

func (t *fakeTester) Test(_ context.Context, target tunneltest.Target) (tunneltest.Report, error) {
	t.ev.add("test")
	t.target = target
	return t.report, t.err
}

type fakePub struct {
	ev *events

	published []*fleet.ConfigBody
	statuses  []StatusBody
	seq       uint64

	// current is what the gist is taken to hold.
	current    *fleet.ConfigBody
	currentErr error

	publishErr error
	inbox      Inbox

	// notSynced makes Reconcile report that a place is behind, and
	// reconcileErr that none could be reached; reconciles counts the calls.
	notSynced    bool
	reconcileErr error
	reconciles   int

	// syncAfter, when set, makes the places agree once Reconcile has been
	// called that many times.
	syncAfter int
}

func (p *fakePub) PublishConfig(_ context.Context, body *fleet.ConfigBody) (uint64, error) {
	p.ev.add("publish")
	if p.publishErr != nil {
		return 0, p.publishErr
	}
	p.seq++
	p.published = append(p.published, body)
	p.current = body
	return p.seq, nil
}

func (p *fakePub) PublishStatus(_ context.Context, s StatusBody) error {
	p.ev.add("status")
	p.statuses = append(p.statuses, s)
	return nil
}

func (p *fakePub) Current(context.Context) (*fleet.ConfigBody, uint64, error) {
	if p.currentErr != nil {
		return nil, 0, p.currentErr
	}
	if p.current == nil {
		return nil, 0, errors.New("nothing published")
	}
	return p.current, p.seq, nil
}

func (p *fakePub) ReadInbox(context.Context) (Inbox, error) { return p.inbox, nil }

func (p *fakePub) Reconcile(context.Context) (SyncState, error) {
	p.ev.add("reconcile")
	p.reconciles++
	if p.reconcileErr != nil {
		return SyncState{Detail: p.reconcileErr.Error()}, p.reconcileErr
	}
	if p.notSynced && (p.syncAfter == 0 || p.reconciles <= p.syncAfter) {
		return SyncState{Detail: "GitLab is behind"}, nil
	}
	return SyncState{Synced: true, Detail: "all places agree"}, nil
}

type fakeHost struct {
	watchLog   []byte
	running    bool
	restarts   int
	status     node.Status
	statusErr  error
	pool, body []byte
	registry   []byte
	access     []byte
	written    []byte
}

func (h *fakeHost) NodeRunning(context.Context) (bool, error) { return h.running, nil }
func (h *fakeHost) RestartNode(context.Context) error         { h.restarts++; return nil }
func (h *fakeHost) NodeStatus(context.Context) (node.Status, error) {
	return h.status, h.statusErr
}
func (h *fakeHost) ReadWatchLog() ([]byte, error) { return h.watchLog, nil }
func (h *fakeHost) ReadPool() ([]byte, error)     { return h.pool, nil }
func (h *fakeHost) ReadRegistry() ([]byte, error) { return h.registry, nil }
func (h *fakeHost) ReadAccess() ([]byte, error)   { return h.access, nil }
func (h *fakeHost) ReadBody() ([]byte, error)     { return h.body, nil }
func (h *fakeHost) WriteBody(b []byte) error      { h.written = b; return nil }

type fakeProber struct {
	target, control Reach
	err             error
	calls           int
}

func (p *fakeProber) Reach(context.Context, string) (Reach, Reach, error) {
	p.calls++
	return p.target, p.control, p.err
}

// ---- the whole arrangement

type rig struct {
	t     *testing.T
	keys  testKeys
	cfg   *Config
	clock *clock
	ev    *events

	cloud  *fakeCloud
	prov   *fakeProv
	tester *fakeTester
	pub    *fakePub
	host   *fakeHost
	prober *fakeProber

	j   *JournalStore
	eng *Engine
}

func newRig(t *testing.T) *rig {
	t.Helper()
	keys := newTestKeys(t)
	cfg := testConfig(t, keys)
	cfg.CreatedAt = t0.Unix()

	ev := &events{}
	clk := &clock{t: t0.Add(time.Hour)}

	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}

	cloud := &fakeCloud{ev: ev, instances: []vultr.Instance{{ID: cfg.InstanceID, MainIP: cfg.IP, Region: cfg.Region}}}
	r := &rig{
		t: t, keys: keys, cfg: cfg, clock: clk, ev: ev,
		cloud:  cloud,
		prov:   &fakeProv{ev: ev, cloud: cloud},
		tester: &fakeTester{ev: ev, report: passingReport()},
		pub:    &fakePub{ev: ev, seq: 10},
		host:   &fakeHost{running: true, pool: []byte(`[{"fake":"198.18.0.2"}]`)},
		prober: &fakeProber{target: Reach{10, 10}, control: Reach{6, 6}},
		j:      j,
	}
	r.host.status = node.Status{UpdatedAt: clk.Now()}

	r.eng = &Engine{
		Cfg: cfg, J: j, Cloud: cloud, Prov: r.prov, Tester: r.tester, Pub: r.pub, Host: r.host,
		Plan: func(context.Context) (*splitplan.Plan, error) {
			return &splitplan.Plan{
				Files:      []deploy.File{{What: "ru", Path: node.DefaultRUSetPath, Mode: "0644", Content: []byte("1.0.0.0-1.0.0.255\n")}},
				DNS:        []string{"198.18.0.1"},
				AllowedIPs: []string{"198.18.0.0/15"},
			}, nil
		},
		Now:   clk.Now,
		Sleep: clk.Sleep,
		Intn:  func(int) int { return 0 },
	}
	return r
}

func (r *rig) manager() *Manager {
	m := NewManager(Manager{
		Cfg: r.cfg, J: r.j, Eng: r.eng, Pub: r.pub, Cloud: r.cloud, Host: r.host, Prober: r.prober,
		Now: r.clock.Now,
	})
	return m
}

// command seals a command the way the operator's tool does.
func (r *rig) command(action, arg string, issuedAt time.Time, issuer string, signer ed25519.PrivateKey) string {
	r.t.Helper()
	id, err := proto.NewCommandID()
	if err != nil {
		r.t.Fatal(err)
	}
	return r.commandWithID(id, action, arg, issuedAt, issuer, signer)
}

func (r *rig) commandWithID(id, action, arg string, issuedAt time.Time, issuer string, signer ed25519.PrivateKey) string {
	r.t.Helper()
	priv := r.keys.root
	if issuer == proto.IssuerBackup {
		priv = r.keys.backup
	}
	if signer != nil {
		priv = signer
	}
	blob, err := proto.SealCommand(&proto.Command{
		ID: id, IssuedAt: issuedAt.Unix(), Action: action, Arg: arg,
	}, issuer, priv, r.keys.content)
	if err != nil {
		r.t.Fatal(err)
	}
	return blob
}

// placeholder is the encrypted empty object a gist file starts with.
func (r *rig) placeholder() string {
	blob, err := proto.Seal(r.keys.content, []byte("{}"))
	if err != nil {
		r.t.Fatal(err)
	}
	return blob
}
