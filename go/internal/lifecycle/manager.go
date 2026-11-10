package lifecycle

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"sync/atomic"
	"time"

	"awgrotator/internal/fleet"
	"awgrotator/internal/proto"
)

// Prober asks the outside world whether this server can be reached.
type Prober interface {
	// Reach returns how many probes in the target country and how many in a
	// control group could reach the address.
	Reach(ctx context.Context, ip string) (target, control Reach, err error)
}

// Intervals between the things the manager does. A zero value means the
// default.
type Intervals struct {
	Local     time.Duration // the server's own services
	External  time.Duration // reachability from outside
	Status    time.Duration // publishing the status
	Heartbeat time.Duration // republishing the configuration

	// Reconcile is how often the places the files are kept are compared when
	// they were last found to agree, and ReconcileBehind how often while one of
	// them is behind or could not be reached.
	Reconcile       time.Duration
	ReconcileBehind time.Duration

	// Watch is how often the availability log gets a line.
	Watch time.Duration
}

// StatusGrace is how long the node's status may be missing or stale before the
// node is taken to be unhealthy. A node writes its first status a moment after
// it starts, and a manager that restarted it for not having one yet would be
// restarting the tunnel out from under its clients.
const StatusGrace = 90 * time.Second

// Defaults for Intervals.
const (
	DefaultLocalEvery     = time.Minute
	DefaultExternalEvery  = 10 * time.Minute
	DefaultStatusEvery    = 24 * time.Hour
	DefaultHeartbeatEvery = 7 * 24 * time.Hour

	DefaultReconcileEvery       = time.Hour
	DefaultReconcileBehindEvery = 5 * time.Minute
)

func (i Intervals) withDefaults() Intervals {
	if i.Local == 0 {
		i.Local = DefaultLocalEvery
	}
	if i.External == 0 {
		i.External = DefaultExternalEvery
	}
	if i.Status == 0 {
		i.Status = DefaultStatusEvery
	}
	if i.Heartbeat == 0 {
		i.Heartbeat = DefaultHeartbeatEvery
	}
	if i.Reconcile == 0 {
		i.Reconcile = DefaultReconcileEvery
	}
	if i.ReconcileBehind == 0 {
		i.ReconcileBehind = DefaultReconcileBehindEvery
	}
	if i.Watch == 0 {
		i.Watch = DefaultWatchEvery
	}
	return i
}

// Manager is the server's own caretaker. Tick is called regularly and does
// whatever has become due.
type Manager struct {
	Cfg    *Config
	J      *JournalStore
	Eng    *Engine
	Pub    Publisher
	Cloud  Cloud
	Host   Host
	Prober Prober

	// Watch and Reverse keep the availability log: the server asks whether it
	// can still get answers from Russia and writes down what it finds. Both are
	// optional, and nothing here acts on what is found.
	Watch   *WatchLog
	Reverse ReverseProber

	Every Intervals
	Now   func() time.Time
	Log   *slog.Logger

	// OnEvent is told what the operator should hear about without asking: a
	// move starting, failing or handing over, this server taking over, the
	// health verdict changing. It is called on the manager's goroutine and
	// must not block.
	OnEvent func(Event)

	// ConfigPath is where the configuration is saved when the operator changes
	// it through the bot. Empty means the change lasts until a restart.
	ConfigPath string

	actions          chan Action
	snap             *atomic.Pointer[Snapshot]
	moving           bool
	current          bool
	currentKnown     bool
	lastCurrentCheck time.Time
	lastVerdict      Verdict

	tracker      *Tracker
	lastLocal    time.Time
	lastExternal time.Time

	// statusBadSince is when the node's status first went missing or stale, so
	// that a node which has only just started is given time to write its first
	// one before it is judged.
	statusBadSince time.Time

	// lastReconcile is when the places were last compared, and behind says that
	// they were not found in step, or that something has been published since,
	// so the next look comes sooner.
	lastReconcile time.Time
	behind        bool

	watch watchState

	pending   *MoveRequest
	statusDue bool
	why       string
	started   bool
}

// NewManager builds a manager.
func NewManager(m Manager) *Manager {
	m.Every = m.Every.withDefaults()
	m.tracker = NewTracker(m.Cfg.Policy.BlockRounds)
	m.why = "starting up"
	m.actions = make(chan Action, 8)
	m.snap = new(atomic.Pointer[Snapshot])
	return &m
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Start does what has to happen once, before the first Tick: it finishes
// whatever a previous run left half done, and sets up the journal.
func (m *Manager) Start(ctx context.Context) error {
	if m.started {
		return nil
	}
	m.started = true

	// The age limit gets a random extra, drawn once and remembered, so a line
	// of servers does not replace itself on a perfectly regular beat.
	jitter, err := drawJitter(m.Cfg.Policy.AgeJitterHours)
	if err != nil {
		return err
	}
	now := m.now()
	if err := m.J.Update(func(j *Journal) {
		if j.AgeJitterSec == 0 {
			j.AgeJitterSec = int64(jitter / time.Second)
		}
		// A new server was just published by its predecessor, so it has
		// nothing to refresh yet.
		if j.LastHeartbeatAt == 0 {
			j.LastHeartbeatAt = m.Cfg.CreatedAt
		}
		if j.LastStatusAt == 0 {
			j.LastStatusAt = now.Unix() - int64(m.Every.Status/time.Second) // publish soon
		}
	}); err != nil {
		return err
	}

	m.statusDue = true
	// A new server looks at once at whether the places agree: it may be the
	// first thing that fills one of them.
	m.behind = true
	// The first look at the node waits a full interval: it was probably
	// started a moment before this manager was.
	m.lastLocal = now

	// An attempt that cannot be settled yet does not stop the manager from
	// starting: it is retried on every pass until it can be.
	if err := m.Eng.Resume(ctx); err != nil {
		m.log().Warn("could not settle an unfinished move at start-up; will retry", "error", err)
	}
	m.publishSnapshot(ctx)
	return nil
}

func drawJitter(hours int) (time.Duration, error) {
	if hours <= 0 {
		return 0, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(hours)*3600))
	if err != nil {
		return 0, err
	}
	return time.Duration(n.Int64()) * time.Second, nil
}

// Tick does whatever is due: it reads the operator's instructions, checks the
// server's health, decides whether to move, and publishes status. It is safe
// to call often.
func (m *Manager) Tick(ctx context.Context) error {
	now := m.now()
	// Whatever way the pass ends, the bot gets to see how things stand.
	defer func() {
		m.checkCurrent(ctx, m.now())
		m.noteHealth()
		m.publishSnapshot(ctx)
	}()

	m.pollInbox(ctx, now)

	if m.revoked() {
		m.log().Error("this server's key has been revoked by the operator; removing this server")
		return m.Eng.DeleteSelf(ctx)
	}

	// A move that was published but not yet retired is finished first.
	if latest := m.latest(); latest != nil && (latest.State == StatePublished || latest.State == StateRetiring) {
		return m.Eng.Retire(ctx, latest.ID)
	}

	// A move that was begun and never finished can only be left over from a
	// previous run, because a move runs to the end inside a single pass. It is
	// settled before anything new is started; this is also how a server that
	// could not be removed yet, because the provider still had it locked, gets
	// removed once it can be.
	if latest := m.latest(); latest != nil &&
		(latest.State == StateStarted || latest.State == StateProvisioned || latest.State == StateVerified) {
		if err := m.Eng.Resume(ctx); err != nil {
			m.log().Warn("an unfinished move is still not settled; will retry", "error", err)
		}
		return nil
	}

	m.checkLocal(ctx, now)
	m.checkExternal(ctx, now)

	if err := m.maybeMove(ctx, now); err != nil {
		return err
	}
	m.maybeWatch(ctx, now)
	m.maybeStatus(ctx, now)
	m.maybeHeartbeat(ctx, now)
	m.maybeReconcile(ctx, now)
	return nil
}

func (m *Manager) latest() *Attempt {
	j := m.J.Read()
	return j.Latest()
}

func (m *Manager) revoked() bool {
	return m.J.Read().MinEpoch > m.Cfg.Cert.Epoch
}

// ---- the operator's instructions

// pollInbox reads the command and revocation files and acts on what is new.
// A file that does not verify is ignored: anyone can write to a gist they
// know the address of, and only the operator's signature counts.
func (m *Manager) pollInbox(ctx context.Context, now time.Time) {
	in, err := m.Pub.ReadInbox(ctx)
	if err != nil {
		m.log().Debug("could not read the inbox", "error", err)
		return
	}
	trust := m.Cfg.Trust()

	if in.Revocation != "" && !m.placeholder(in.Revocation) {
		j := m.J.Read()
		if _, next, err := trust.OpenRevocation(in.Revocation, now, proto.State{MinEpoch: j.MinEpoch}); err == nil {
			m.applyMinEpoch(next.MinEpoch)
		}
	}

	if in.Command == "" || m.placeholder(in.Command) {
		return
	}
	cmd, err := trust.OpenCommand(in.Command, now)
	if err != nil {
		m.log().Debug("ignoring a command that does not verify", "error", err)
		return
	}
	if m.J.Read().WasExecuted(cmd.ID) {
		return
	}
	// Recorded before it is acted on: a command that fails must not be
	// retried forever, and a repeat of it is the operator's to send.
	if err := m.J.Update(func(j *Journal) { j.MarkExecuted(cmd.ID) }); err != nil {
		m.log().Error("could not record a command; ignoring it", "error", err)
		return
	}

	m.log().Info("received a command", "action", cmd.Action, "issuer", cmd.Issuer)
	switch cmd.Action {
	case proto.ActionMove:
		m.pending = &MoveRequest{Trigger: TriggerCommand}
	case proto.ActionMoveRegion:
		m.pending = &MoveRequest{Trigger: TriggerCommand, Region: cmd.Arg}
	case proto.ActionStatus:
		m.statusDue = true
	case proto.ActionRevoke:
		epoch, err := strconv.ParseUint(cmd.Arg, 10, 32)
		if err != nil {
			m.log().Warn("a revoke command has no usable epoch", "arg", cmd.Arg)
			return
		}
		m.applyMinEpoch(uint32(epoch))
	}
}

func (m *Manager) applyMinEpoch(epoch uint32) {
	if epoch <= m.J.Read().MinEpoch {
		return
	}
	if err := m.J.Update(func(j *Journal) { j.MinEpoch = epoch }); err != nil {
		m.log().Error("could not record a revocation", "error", err)
		return
	}
	m.log().Warn("the operator raised the minimum certificate epoch", "min_epoch", epoch)
}

// placeholder reports whether a gist file holds the empty placeholder it is
// created with rather than something the operator wrote.
func (m *Manager) placeholder(blob string) bool {
	key, err := m.Cfg.ContentKeyBytes()
	if err != nil {
		return false
	}
	plain, err := proto.Open(key, blob)
	return err == nil && string(plain) == "{}"
}

// ---- health

// checkLocal verifies the server's own services and restarts the tunnel if it
// has stopped.
func (m *Manager) checkLocal(ctx context.Context, now time.Time) {
	if !m.lastLocal.IsZero() && now.Sub(m.lastLocal) < m.Every.Local {
		return
	}
	m.lastLocal = now

	running, err := m.Host.NodeRunning(ctx)
	reason := "the service is not running"
	if err != nil {
		reason = err.Error()
	}
	if err == nil && running {
		_, statusErr := m.Host.NodeStatus(ctx)
		if statusErr == nil {
			m.statusBadSince = time.Time{}
			m.tracker.Local(true)
			return
		}

		// Running but not reporting. Give it time: it may have just started.
		if m.statusBadSince.IsZero() {
			m.statusBadSince = now
			m.log().Debug("the node is running but has no fresh status yet", "error", statusErr)
			return
		}
		if now.Sub(m.statusBadSince) < StatusGrace {
			return
		}
		reason = "it is running but has stopped reporting: " + statusErr.Error()
	}

	m.statusBadSince = time.Time{}
	m.log().Warn("the tunnel service is not healthy; restarting it", "reason", reason)
	if err := m.Host.RestartNode(ctx); err != nil {
		m.log().Error("could not restart the tunnel service", "error", err)
	}
	m.tracker.Local(false)
}

// checkExternal asks the outside world whether the server can be reached from
// the target country.
func (m *Manager) checkExternal(ctx context.Context, now time.Time) {
	if m.Prober == nil {
		return
	}
	if !m.lastExternal.IsZero() && now.Sub(m.lastExternal) < m.Every.External {
		return
	}
	m.lastExternal = now

	target, control, err := m.Prober.Reach(ctx, m.Cfg.IP)
	if err != nil {
		m.log().Debug("the external check could not run", "error", err)
		m.tracker.External(Inconclusive)
		m.noteExternal(now, Reach{}, Reach{}, Inconclusive, err)
		return
	}
	round := Classify(target, control)
	m.tracker.External(round)
	m.noteExternal(now, target, control, round, nil)
	if round == Bad {
		m.log().Warn("the target country cannot reach this server",
			"reached", target.Reached, "of", target.Total, "control", control.Reached,
			"bad_rounds", m.tracker.BadRounds())
	}
}

// ---- moving

func (m *Manager) maybeMove(ctx context.Context, now time.Time) error {
	j := m.J.Read()
	decision := Decide(m.Cfg.Policy, Inputs{
		Now:       now,
		CreatedAt: m.Cfg.CreatedTime(),
		AgeJitter: time.Duration(j.AgeJitterSec) * time.Second,
		Attempts:  j.AttemptTimes(),
		Health:    m.tracker.Verdict(),
		Command:   m.pending,
	})
	m.why = decision.Reason
	if !decision.Move {
		return nil
	}

	m.log().Info("moving", "trigger", decision.Request.Trigger, "why", decision.Reason)
	m.pending = nil
	m.moving = true
	m.publishSnapshot(ctx)
	m.emit(Event{Kind: EventMoveStarted, Trigger: decision.Request.Trigger, Why: decision.Reason, Region: decision.Request.Region})
	res, err := m.Eng.Move(ctx, decision.Request)
	m.moving = false
	if err != nil {
		m.statusDue = true
		// A failed move leaves this server running. The failure is in the
		// journal and goes into the next status; Tick does not fail on it,
		// or the process would be restarted into trying again at once.
		m.log().Error("the move did not succeed; this server stays", "error", err)
		m.emit(Event{Kind: EventMoveFailed, Trigger: decision.Request.Trigger, Err: err.Error()})
		return nil
	}
	// Handed over: from now on the replacement answers the operator. Told
	// before the grace period is waited out, which takes a while.
	m.current = false
	m.publishSnapshot(ctx)
	m.emit(Event{Kind: EventMovePublished, Trigger: decision.Request.Trigger, Region: res.Region})
	return m.Eng.Retire(ctx, res.AttemptID)
}

// ---- publishing

func (m *Manager) maybeStatus(ctx context.Context, now time.Time) {
	last := time.Unix(m.J.Read().LastStatusAt, 0)
	// A change in what the watch sees is worth publishing soon, but not at the
	// rate it may flap.
	watchWorthIt := m.watch.due && now.Sub(last) >= minStatusGap
	if !m.statusDue && !watchWorthIt && now.Sub(last) < m.Every.Status {
		return
	}
	status, err := m.statusBody(ctx, now)
	if err != nil {
		m.log().Warn("could not assemble the status", "error", err)
		return
	}
	if err := m.Pub.PublishStatus(ctx, status); err != nil {
		m.log().Warn("could not publish the status", "error", err)
		return
	}
	m.statusDue = false
	m.watch.due = false
	m.behind = true
	_ = m.J.Update(func(j *Journal) { j.LastStatusAt = now.Unix() })
}

func (m *Manager) statusBody(ctx context.Context, now time.Time) (StatusBody, error) {
	j := m.J.Read()
	s := StatusBody{
		Version:      StatusVersion,
		Generation:   m.Cfg.Generation,
		Region:       m.Cfg.Region,
		AgeHours:     int(now.Sub(m.Cfg.CreatedTime()) / time.Hour),
		AutoAge:      m.Cfg.Policy.AutoAge,
		AutoBlock:    m.Cfg.Policy.AutoBlock,
		Health:       m.tracker.Verdict(),
		Why:          m.why,
		CertDaysLeft: m.Cfg.CertDaysLeft(now),
		LastMove:     SummaryOf(j.Latest()),
		Revoked:      j.MinEpoch > m.Cfg.Cert.Epoch,
		Watch:        m.watchSummary(),
		UpdatedAt:    now.Unix(),
	}
	if next, ok := NextAutoMove(m.Cfg.Policy, m.Cfg.CreatedTime(), time.Duration(j.AgeJitterSec)*time.Second); ok {
		s.NextAutoMove = next.Unix()
	}
	if tagged, err := m.Cloud.Tagged(ctx); err == nil {
		s.Servers = len(tagged)
	}
	if st, err := m.Host.NodeStatus(ctx); err == nil {
		s.Node = Summarize(st)
	}
	return s, nil
}

// maybeHeartbeat republishes the configuration now and then, so that clients
// can see that a server is alive and the documents never expire.
//
// It only does so while this server is the one that is published. A server
// that has been replaced and is waiting out its grace period must not
// overwrite the configuration of the server that took over.
func (m *Manager) maybeHeartbeat(ctx context.Context, now time.Time) {
	last := time.Unix(m.J.Read().LastHeartbeatAt, 0)
	if now.Sub(last) < m.Every.Heartbeat {
		return
	}

	current, _, err := m.Pub.Current(ctx)
	if err != nil {
		m.log().Warn("not republishing: cannot read what is published", "error", err)
		return
	}
	if endpointHost(current.Endpoint) != m.Cfg.IP {
		m.log().Warn("not republishing: the published configuration points at another server",
			"published", endpointHost(current.Endpoint), "this", m.Cfg.IP)
		return
	}

	raw, err := m.Host.ReadBody()
	if err != nil || len(raw) == 0 {
		m.log().Warn("not republishing: no saved configuration", "error", err)
		return
	}
	body, err := fleet.ParseBody(raw)
	if err != nil {
		m.log().Warn("not republishing: the saved configuration is not usable", "error", err)
		return
	}
	seq, err := m.Pub.PublishConfig(ctx, body)
	if err != nil {
		m.log().Warn("could not republish the configuration", "error", err)
		return
	}
	m.log().Info("republished the configuration", "seq", seq)
	m.behind = true
	_ = m.J.Update(func(j *Journal) { j.LastHeartbeatAt = now.Unix() })
}

// maybeReconcile compares the places the files are kept and brings the one that
// is behind up to date. A place can miss a publication, or the operator's
// announcement, simply because it was out of reach at that moment; this is what
// makes them mirrors of each other anyway. While one is behind it looks again
// every few minutes, and otherwise now and then.
func (m *Manager) maybeReconcile(ctx context.Context, now time.Time) {
	every := m.Every.Reconcile
	if m.behind {
		every = m.Every.ReconcileBehind
	}
	if !m.lastReconcile.IsZero() && now.Sub(m.lastReconcile) < every {
		return
	}
	m.lastReconcile = now

	st, err := m.Pub.Reconcile(ctx)
	if err != nil {
		m.behind = true
		m.log().Warn("could not compare the places the configuration is kept in", "error", err)
		return
	}
	m.behind = !st.Synced
	switch {
	case !st.Synced:
		m.log().Warn("the places the configuration is kept in are not in step yet; trying again soon", "detail", st.Detail)
	case st.Wrote:
		m.log().Info("brought the places the configuration is kept in into step", "detail", st.Detail)
	default:
		m.log().Debug("the places the configuration is kept in agree")
	}
}

// Verdict returns the current health verdict, for a caller that wants to
// report it.
func (m *Manager) Verdict() Verdict { return m.tracker.Verdict() }

// ErrNoPending is returned by helpers that need a queued move and find none.
var ErrNoPending = errors.New("no move is pending")

// Pending returns the move the operator has asked for and that has not yet
// started, or an error if there is none.
func (m *Manager) Pending() (MoveRequest, error) {
	if m.pending == nil {
		return MoveRequest{}, ErrNoPending
	}
	return *m.pending, nil
}

// String describes the manager for a log line.
func (m *Manager) String() string {
	return fmt.Sprintf("manager(gen %d, %s)", m.Cfg.Generation, m.Cfg.Region)
}
