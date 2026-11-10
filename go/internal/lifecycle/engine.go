package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/netip"
	"path/filepath"
	"time"

	"awgrotator/internal/deploy"
	"awgrotator/internal/enroll"
	"awgrotator/internal/fleet"
	"awgrotator/internal/node"
	"awgrotator/internal/splitplan"
	"awgrotator/internal/tunneltest"
	"awgrotator/internal/vultr"
)

// Cloud is the part of the cloud provider the engine talks to directly.
type Cloud interface {
	// Tagged lists the instances carrying our tag, and only those.
	Tagged(ctx context.Context) ([]vultr.Instance, error)

	// Delete destroys an instance.
	Delete(ctx context.Context, id string) error
}

// ProvisionRequest describes a server to build.
type ProvisionRequest struct {
	Region string
	Label  string
	Node   *node.Config
	Files  []deploy.File

	// FilesFor supplies files that need the instance to exist first.
	FilesFor func(inst *vultr.Instance) ([]deploy.File, error)

	// OnCreated is called the moment the instance exists, so that it can be
	// written down before the slow part of building it begins.
	OnCreated func(inst *vultr.Instance)
}

// Provisioner creates a server and installs the node and manager on it. A
// failure leaves nothing behind: whatever was created is removed.
type Provisioner interface {
	Provision(ctx context.Context, req ProvisionRequest) (*deploy.Result, error)
}

// Tester connects to a server as a client would and checks the whole path.
type Tester interface {
	Test(ctx context.Context, target tunneltest.Target) (tunneltest.Report, error)
}

// Host is what the engine and manager need from the machine they run on.
type Host interface {
	// NodeRunning reports whether the tunnel service is up.
	NodeRunning(ctx context.Context) (bool, error)

	// RestartNode restarts the tunnel service.
	RestartNode(ctx context.Context) error

	// NodeStatus reads the node's status. It is an error if the status is
	// missing or has gone stale, which means the node is not running properly.
	NodeStatus(ctx context.Context) (node.Status, error)

	// ReadPool returns the node's saved table of stand-in addresses, or nil
	// if there is none yet.
	ReadPool() ([]byte, error)

	// ReadRegistry returns the node's list of registered devices, or nil if
	// there is none yet.
	ReadRegistry() ([]byte, error)

	// ReadWatchLog returns the end of the availability log, or nil if there is
	// none, for a replacement to carry on from.
	ReadWatchLog() ([]byte, error)

	// ReadBody and WriteBody keep the configuration this server publishes, so
	// that it can be republished.
	ReadBody() ([]byte, error)
	WriteBody(body []byte) error
}

// ErrCapReached is returned when moving would exceed the server ceiling.
var ErrCapReached = errors.New("the server ceiling has been reached")

// Engine builds a replacement server, proves it works, publishes it and then
// retires the server it runs on.
//
// Everything that can go wrong before the new configuration is published ends
// the same way: the new server is removed and the current one carries on.
// Nothing is deleted that this attempt did not create, except the server the
// engine itself runs on, and only after clients have been told where to go.
type Engine struct {
	Cfg    *Config
	J      *JournalStore
	Cloud  Cloud
	Prov   Provisioner
	Tester Tester
	Pub    Publisher
	Host   Host

	// Plan prepares the address set, rules and routes for a new server.
	Plan func(ctx context.Context) (*splitplan.Plan, error)

	Now func() time.Time

	// Sleep waits, and returns early with an error if the context ends. It is
	// replaced in tests so that grace periods do not have to be waited out.
	Sleep func(ctx context.Context, d time.Duration) error

	// Intn picks a random number below n. Replaced in tests.
	Intn func(n int) int

	Log *slog.Logger
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

// MoveResult describes the server that now takes over.
type MoveResult struct {
	AttemptID  string
	InstanceID string
	IP         string
	Region     string
	Seq        uint64
}

// Move builds, tests and publishes a replacement. It does not retire the
// current server: the caller does that with Retire once Move has returned, so
// that publishing and retiring are separate steps with a deliberate gap.
func (e *Engine) Move(ctx context.Context, req MoveRequest) (*MoveResult, error) {
	id, err := newAttemptID()
	if err != nil {
		return nil, err
	}
	if err := e.J.Begin(Attempt{
		ID: id, StartedAt: e.now().Unix(), Trigger: req.Trigger, State: StateStarted,
	}); err != nil {
		return nil, fmt.Errorf("record the attempt: %w", err)
	}
	fail := func(err error) (*MoveResult, error) {
		_ = e.J.Step(id, func(a *Attempt) { a.State = StateFailed; a.Error = err.Error() })
		e.log().Warn("move failed", "trigger", req.Trigger, "error", err)
		return nil, err
	}

	// 1. Room. Counting only what carries our tag, so nothing else on the
	// account is ever in the way, and nothing else is ever touched.
	tagged, err := e.Cloud.Tagged(ctx)
	if err != nil {
		return fail(fmt.Errorf("list servers: %w", err))
	}
	if len(tagged) >= e.Cfg.Policy.MaxServers {
		return fail(fmt.Errorf("%w: %d servers exist, the limit is %d",
			ErrCapReached, len(tagged), e.Cfg.Policy.MaxServers))
	}

	// 2. Where.
	region, err := e.chooseRegion(req.Region)
	if err != nil {
		return fail(err)
	}
	if err := e.J.Step(id, func(a *Attempt) { a.Region = region }); err != nil {
		return fail(err)
	}

	// 3. What. Fresh keys and parameters, and a freshly refreshed address set.
	contentKey, err := e.Cfg.ContentKeyBytes()
	if err != nil {
		return fail(fmt.Errorf("content key: %w", err))
	}
	gen, err := fleet.NewGeneration(fleet.GenOptions{Split: true, ContentKey: contentKey})
	if err != nil {
		return fail(err)
	}
	plan, err := e.Plan(ctx)
	if err != nil {
		return fail(fmt.Errorf("prepare split routing: %w", err))
	}
	clientNet := fleet.ClientNet{DNS: plan.DNS, AllowedIPs: plan.AllowedIPs}

	// 4. Build it.
	var body *fleet.ConfigBody
	res, err := e.Prov.Provision(ctx, ProvisionRequest{
		Region: region,
		Label:  fmt.Sprintf("awg-%s-g%d-%d", region, e.Cfg.Generation+1, e.now().Unix()),
		Node:   gen.Node,
		Files:  plan.Files,
		// Written down at once. If this process dies during the minutes it
		// takes to install the server, Resume can still find it and clean up.
		OnCreated: func(inst *vultr.Instance) {
			if err := e.J.Step(id, func(a *Attempt) { a.InstanceID, a.NewIP = inst.ID, inst.MainIP }); err != nil {
				e.log().Error("could not record the new server; if this process dies it may be left running",
					"instance", inst.ID, "error", err)
			}
		},
		FilesFor: func(inst *vultr.Instance) ([]deploy.File, error) {
			// The body needs the new server's address, which exists only
			// now. It is built here and published later, unchanged.
			body = gen.Body(region, inst.MainIP, clientNet, e.now())
			return e.successorFiles(inst, region, body)
		},
	})
	if err != nil {
		// Provisioning removes anything it created before failing.
		return fail(fmt.Errorf("build the new server: %w", err))
	}
	if body == nil {
		body = gen.Body(region, res.IP, clientNet, e.now())
	}
	if err := e.J.Step(id, func(a *Attempt) {
		a.State, a.InstanceID, a.NewIP = StateProvisioned, res.InstanceID, res.IP
	}); err != nil {
		_ = e.removeSuccessor(ctx, res.InstanceID)
		return fail(err)
	}

	// From here the new server exists and nobody knows about it, so every
	// failure removes it. If it cannot be removed, the attempt is left open
	// rather than closed as failed: closing it would make the journal claim
	// the server is gone, and nothing would ever try again.
	abandon := func(cause error) (*MoveResult, error) {
		if rmErr := e.removeSuccessor(ctx, res.InstanceID); rmErr != nil {
			_ = e.J.Step(id, func(a *Attempt) {
				a.Error = cause.Error() + "; the new server could not be removed yet: " + rmErr.Error()
			})
			return nil, fmt.Errorf("%w (the new server could not be removed yet and will be retried: %v)", cause, rmErr)
		}
		return fail(cause)
	}

	// 5. Prove it: through its tunnel, with its parameters, the way a client
	// would use it.
	target, err := e.testTarget(gen, res.IP, contentKey)
	if err != nil {
		return abandon(err)
	}
	report, err := e.Tester.Test(ctx, target)
	if err != nil {
		return abandon(fmt.Errorf("test the new server: %w", err))
	}
	if !report.Passed() {
		return abandon(fmt.Errorf("the new server failed the tunnel test:\n%s", report))
	}
	e.log().Info("the new server passed the tunnel test", "region", region)
	if err := e.J.Step(id, func(a *Attempt) { a.State = StateVerified }); err != nil {
		return abandon(err)
	}

	// 6. Tell the clients.
	seq, err := e.Pub.PublishConfig(ctx, body)
	if err != nil {
		return abandon(fmt.Errorf("publish: %w", err))
	}
	published := e.now()
	if err := e.J.Step(id, func(a *Attempt) {
		a.State = StatePublished
		a.PublishedAt = published.Unix()
		a.RetireAt = published.Add(e.Cfg.Policy.Grace()).Unix()
	}); err != nil {
		// Published but not recorded. Do NOT remove the server: clients are
		// already being sent to it. Resume works out what happened from the
		// gist if the process restarts.
		e.log().Error("published, but could not record it", "error", err)
	}

	e.log().Info("the new server is published", "region", region, "seq", seq)
	return &MoveResult{AttemptID: id, InstanceID: res.InstanceID, IP: res.IP, Region: region, Seq: seq}, nil
}

// removeSuccessor deletes a server this engine created and is abandoning. It
// uses a context that survives cancellation, so an interrupted move still
// cleans up after itself.
//
// It reports whether the server is gone. A caller must not treat an attempt as
// cleaned up when it is not: the provider refuses to delete a server that is
// still being installed ("locked"), which is exactly the state an abandoned
// server is in when the process that was building it has just died.
func (e *Engine) removeSuccessor(ctx context.Context, instanceID string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := e.Cloud.Delete(cleanup, instanceID); err != nil {
		e.log().Error("could not remove the abandoned server yet: it is still running and costing money",
			"instance", instanceID, "error", err)
		return err
	}
	e.log().Info("removed the abandoned server", "instance", instanceID)
	return nil
}

// chooseRegion picks where the new server goes. An explicit request must name
// a known region; otherwise any region but the current one is chosen, so that a
// move actually changes something.
func (e *Engine) chooseRegion(want string) (string, error) {
	known := e.Cfg.Regions
	if want != "" {
		for _, r := range known {
			if r == want {
				return want, nil
			}
		}
		return "", fmt.Errorf("unknown region %q, choose from %v", want, known)
	}

	var others []string
	for _, r := range known {
		if r != e.Cfg.Region {
			others = append(others, r)
		}
	}
	if len(others) == 0 {
		others = known
	}
	return others[e.intn(len(others))], nil
}

func (e *Engine) intn(n int) int {
	if e.Intn != nil {
		return e.Intn(n)
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// testTarget describes the new server for the tunnel test.
func (e *Engine) testTarget(gen *fleet.Generation, ip string, contentKey []byte) (tunneltest.Target, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return tunneltest.Target{}, fmt.Errorf("the new server's address %q: %w", ip, err)
	}
	endpoint := netip.AddrPortFrom(addr, uint16(gen.Port))
	return tunneltest.Target{
		Endpoint:      endpoint,
		ServerPublic:  gen.Server.Public,
		Params:        gen.Params,
		ClientPrivate: gen.Health.Private,
		ClientAddr:    netip.MustParseAddr("10.8.0.250"),
		Resolver:      netip.AddrPortFrom(netip.MustParseAddr(node.DefaultDNSAddress), 53),
		StandIn:       netip.MustParsePrefix(node.DefaultStandInRange),

		// A server that cannot take on a new device is not ready to be
		// published, however well the rest of it works.
		Registration: &tunneltest.Registration{
			Endpoint:     endpoint,
			ServerPublic: gen.Server.Public,
			Params:       gen.Params,
			PeerPrivate:  gen.Enroll.Private,
			PeerAddr:     netip.MustParseAddr(node.DefaultEnrollPeerAddress),
			ContentKey:   contentKey,
		},
	}, nil
}

// successorFiles are the files that make a new server a manager in its own
// right: its configuration, the configuration it publishes, and the table of
// stand-in addresses so that addresses clients already cached keep working.
func (e *Engine) successorFiles(inst *vultr.Instance, region string, body *fleet.ConfigBody) ([]deploy.File, error) {
	next := *e.Cfg
	next.InstanceID = inst.ID
	next.IP = inst.MainIP
	next.Region = region
	next.CreatedAt = e.now().Unix()
	next.Generation = e.Cfg.Generation + 1

	cfgJSON, err := next.Marshal()
	if err != nil {
		return nil, err
	}
	bodyJSON, err := body.Marshal()
	if err != nil {
		return nil, err
	}

	// The successor starts with a journal that already knows which commands
	// have been carried out. Without it, the command that caused this very
	// move would still be sitting in the gist when the successor first looks,
	// and it would carry it out again, and so would its successor, and so on
	// until the command went stale. This was found by running it for real.
	seed, err := json.MarshalIndent(e.inheritedJournal(), "", "  ")
	if err != nil {
		return nil, err
	}

	files := []deploy.File{
		{What: "настройки менеджера", Path: DefaultConfigPath, Mode: "0600", Content: cfgJSON},
		{What: "опубликованный конфиг", Path: filepath.Join(next.Data(), "body.json"), Mode: "0600", Content: bodyJSON},
		{What: "журнал: что уже сделано", Path: filepath.Join(next.Data(), "journal.json"), Mode: "0600", Content: append(seed, '\n')},
	}
	if pool, err := e.Host.ReadPool(); err != nil {
		e.log().Warn("could not read the table of stand-in addresses; the new server starts with none", "error", err)
	} else if len(pool) > 0 {
		files = append(files, deploy.File{
			What: "таблица подставных адресов", Path: node.DefaultPoolPath, Mode: "0600", Content: pool,
		})
	}
	// The availability log goes too: the server that is replaced because it was
	// blocked is deleted soon after, and what it saw is the record that matters.
	if raw, err := e.Host.ReadWatchLog(); err != nil {
		e.log().Warn("could not read the availability log; the new server starts a fresh one", "error", err)
	} else if len(raw) > 0 {
		files = append(files, deploy.File{
			What: "журнал доступности", Path: filepath.Join(next.Data(), WatchLogName), Mode: "0600", Content: raw,
		})
	}
	if devices := e.inheritedDevices(); len(devices) > 0 {
		files = append(files, deploy.File{
			What: "список устройств", Path: enroll.DefaultRegistryPath, Mode: "0600", Content: devices,
		})
	}
	return files, nil
}

// inheritedDevices is the list of registered devices, handed to the replacement
// so that every device that works today keeps working after the move without
// registering again. A list that cannot be read or does not check out is left
// behind with a warning: the devices in it register again by themselves, which
// costs them a few seconds, while a bad list would stop the new server starting.
func (e *Engine) inheritedDevices() []byte {
	raw, err := e.Host.ReadRegistry()
	if err != nil {
		e.log().Warn("could not read the device list; the new server starts with none", "error", err)
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	pool, err := enroll.NewPool(fleet.Subnet, 2, 249)
	if err == nil {
		_, err = enroll.ParseRegistry(raw, pool)
	}
	if err != nil {
		e.log().Warn("the device list does not check out; the new server starts with none", "error", err)
		return nil
	}
	return raw
}

// inheritedJournal is the part of this server's memory that a replacement must
// have from its first moment: what has already been done on the operator's
// instruction, and how far revocation and publication have got. Its own
// history of attempts stays behind, and its age limit is drawn afresh.
func (e *Engine) inheritedJournal() Journal {
	j := e.J.Read()
	return Journal{
		Executed: j.Executed,
		MinEpoch: j.MinEpoch,
		LastSeq:  j.LastSeq,
	}
}

// MaxLinger is the longest an old server is kept after its successor has been
// published, however the places the configuration is kept in are doing.
const MaxLinger = time.Hour

// Retire waits out the grace period for the attempt and then deletes the
// server it runs on.
//
// It leaves early only once a minimum time has passed and nobody has been
// connected for a while, so that clients are never cut off just to save a few
// minutes of rent.
func (e *Engine) Retire(ctx context.Context, attemptID string) error {
	j := e.J.Read()
	var attempt *Attempt
	for i := range j.Attempts {
		if j.Attempts[i].ID == attemptID {
			attempt = &j.Attempts[i]
		}
	}
	if attempt == nil {
		return fmt.Errorf("no attempt %s to retire after", attemptID)
	}
	if attempt.State != StatePublished && attempt.State != StateRetiring {
		return fmt.Errorf("attempt %s is %s, not published: refusing to retire", attemptID, attempt.State)
	}

	if err := e.J.Step(attemptID, func(a *Attempt) { a.State = StateRetiring }); err != nil {
		return err
	}
	retireAt := time.Unix(attempt.RetireAt, 0)
	earliest := time.Unix(attempt.PublishedAt, 0).Add(e.Cfg.Policy.MinGrace())
	giveUpAt := time.Unix(attempt.PublishedAt, 0).Add(MaxLinger)

	for {
		now := e.now()
		due := !now.Before(retireAt)
		if !due && !now.Before(earliest) {
			if idle, err := e.idle(ctx); err == nil && idle {
				e.log().Info("nobody is connected; retiring before the grace period ends")
				due = true
			}
		}
		if due {
			// The old server goes only once every place the clients read from
			// has the new configuration: a client that can reach only one of
			// them would otherwise be left with the address of a server that is
			// gone. A place that stays behind does not keep the old server
			// forever, though: a move happens because this server is not
			// serving anyone any more, so after MaxLinger it goes regardless.
			st, err := e.Pub.Reconcile(ctx)
			if err == nil && st.Synced {
				break
			}
			if !now.Before(giveUpAt) {
				e.log().Warn("retiring although not every place has the new configuration", "detail", st.Detail, "error", err)
				break
			}
			e.log().Info("waiting for every place to have the new configuration before retiring", "detail", st.Detail, "error", err)
		}
		wait := 30 * time.Second
		if !due {
			wait = min(wait, retireAt.Sub(now))
		}
		if err := e.sleep(ctx, wait); err != nil {
			return err
		}
	}
	if err := e.DeleteSelf(ctx); err != nil {
		return err
	}
	// If the machine is destroyed straight away this write never lands, which
	// is fine: the state stays "retiring" on a disk that no longer exists.
	_ = e.J.Step(attemptID, func(a *Attempt) { a.State = StateRetired })
	return nil
}

// idle reports whether no client has completed a handshake recently.
func (e *Engine) idle(ctx context.Context) (bool, error) {
	status, err := e.Host.NodeStatus(ctx)
	if err != nil {
		return false, err
	}
	return !status.AnyConnected(), nil
}

// DeleteSelf destroys the server this engine runs on. When it succeeds the
// process is about to stop existing.
func (e *Engine) DeleteSelf(ctx context.Context) error {
	e.log().Info("deleting this server", "instance", e.Cfg.InstanceID)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := e.Cloud.Delete(cleanup, e.Cfg.InstanceID); err != nil {
		return fmt.Errorf("delete this server: %w", err)
	}
	return nil
}

// Resume deals with whatever a previous run left unfinished. It runs at
// start-up, because a restart in the middle of a move is exactly when
// something went wrong and a server might be left behind or be about to be
// deleted.
func (e *Engine) Resume(ctx context.Context) error {
	j := e.J.Read()
	a := j.Latest()
	if a == nil {
		return nil
	}

	switch a.State {
	case StateStarted:
		// A server may exist even though the attempt never got past "started":
		// it was created, recorded, and then this process died while it was
		// being installed. It is dealt with like any other unpublished one.
		if a.InstanceID != "" {
			return e.resumeUnpublished(ctx, a)
		}
		return e.J.Step(a.ID, func(x *Attempt) { x.State = StateFailed; x.Error = "interrupted before anything was created" })

	case StateProvisioned, StateVerified:
		return e.resumeUnpublished(ctx, a)

	case StatePublished, StateRetiring:
		e.log().Info("resuming the retirement of this server", "attempt", a.ID)
		return e.Retire(ctx, a.ID)
	}
	return nil
}

// resumeUnpublished decides what to do about a successor that was built but
// not recorded as published.
//
// The gist is consulted before anything is deleted. The process may have died
// between publishing and writing that down, in which case the successor is
// live, clients are on their way to it, and deleting it would be the worst
// thing to do. If the gist cannot be read, nothing is deleted: a server left
// running for a while costs a few cents, a live one deleted costs the service.
func (e *Engine) resumeUnpublished(ctx context.Context, a *Attempt) error {
	current, _, err := e.Pub.Current(ctx)
	if err != nil {
		e.log().Warn("cannot tell whether the unfinished move was published; leaving the new server alone",
			"attempt", a.ID, "error", err)
		return nil
	}

	if endpointHost(current.Endpoint) == a.NewIP && a.NewIP != "" {
		e.log().Info("the unfinished move had in fact been published; carrying on with it", "attempt", a.ID)
		published := e.now()
		if err := e.J.Step(a.ID, func(x *Attempt) {
			x.State = StatePublished
			x.PublishedAt = published.Unix()
			x.RetireAt = published.Add(e.Cfg.Policy.Grace()).Unix()
		}); err != nil {
			return err
		}
		return e.Retire(ctx, a.ID)
	}

	if a.InstanceID != "" {
		if err := e.removeSuccessor(ctx, a.InstanceID); err != nil {
			// Left open, so the manager tries again on its next pass. The
			// provider will not delete a server while it is still being
			// installed, so the first attempt after a crash often fails.
			return fmt.Errorf("could not remove the unfinished new server yet; will retry: %w", err)
		}
	}
	return e.J.Step(a.ID, func(x *Attempt) {
		x.State = StateFailed
		x.Error = "interrupted before publishing; the unused new server was removed"
	})
}

func endpointHost(endpoint string) string {
	if ap, err := netip.ParseAddrPort(endpoint); err == nil {
		return ap.Addr().String()
	}
	return endpoint
}

func newAttemptID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
