package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// The operator's bot runs on its own goroutine next to the manager. It never
// touches the manager's state: it reads a snapshot the manager publishes after
// each step, and hands over what the operator asked for as actions, which the
// manager carries out on its own goroutine between ticks. That keeps one writer
// for everything the manager owns, the same as before the bot existed.

// Snapshot is the manager's state as the operator is shown it.
type Snapshot struct {
	At           time.Time
	Generation   int
	Region       string
	CreatedAt    time.Time
	Health       Verdict
	Why          string
	AutoAge      bool
	AutoBlock    bool
	NextAutoMove time.Time // zero when moves by age are off
	CertDaysLeft int
	LastMove     *MoveSummary
	Watch        *WatchSummary
	Node         *NodeSummary
	Regions      []string

	// Moving says a move is under way on this server right now.
	Moving bool

	// Current says this server is the one the apps are told to use. Only that
	// server runs the bot. Known says whether that has been found out yet.
	Current      bool
	CurrentKnown bool
}

// ActionKind is what the operator asked for.
type ActionKind string

// Actions.
const (
	ActMove        ActionKind = "move"    // move now, to Region if given
	ActRestartNode ActionKind = "restart" // restart the tunnel service
	ActPolicy      ActionKind = "policy"  // switch automatic moves on or off
	ActCheck       ActionKind = "check"   // ask now whether Russia can reach the server
)

// Action is one thing the operator asked for.
type Action struct {
	Kind ActionKind

	// ID makes the action happen at most once along the whole line of servers:
	// it is written down before it is carried out, and the record goes to the
	// replacement with the rest of the journal. A button that reaches two
	// servers, because the bot moved over in between, acts once.
	ID string

	Region    string // ActMove
	AutoAge   *bool  // ActPolicy
	AutoBlock *bool  // ActPolicy

	// Done is told the outcome, on the manager's goroutine. It must not block.
	Done func(Result)
}

// Result is how an action came out.
type Result struct {
	Err error

	// Skipped says the action had already been carried out.
	Skipped bool

	// Check is the outcome of ActCheck.
	Check *CheckResult
}

// CheckResult is one external check, as the operator asked for it.
type CheckResult struct {
	Target, Control Reach
	Round           Round
}

// EventKind is something the operator is told about without asking.
type EventKind string

// Events.
const (
	EventMoveStarted   EventKind = "move_started"
	EventMoveFailed    EventKind = "move_failed"
	EventMovePublished EventKind = "move_published" // this server handed over
	EventCurrent       EventKind = "current"        // this server took over
	EventHealth        EventKind = "health"
)

// Event is one such thing.
type Event struct {
	Kind       EventKind
	Trigger    Trigger
	Why        string
	Region     string
	Generation int
	Health     Verdict
	Err        string
}

// ErrBusy is returned when the operator's actions are coming faster than the
// manager can take them.
var ErrBusy = errors.New("the manager is busy; try again in a moment")

// How often the manager asks whether it is the server the apps use. A server
// that is not yet looks often, so a new one takes over the bot soon after it
// is published; one that is looks seldom, because only its own move can change
// that, and it knows about its own moves.
const (
	currentCheckWaiting = time.Minute
	currentCheckSettled = 15 * time.Minute
)

// Snapshot returns the state last published. Safe from any goroutine.
func (m *Manager) Snapshot() Snapshot {
	if s := m.snap.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Do queues an action for the manager. Safe from any goroutine.
func (m *Manager) Do(a Action) error {
	select {
	case m.actions <- a:
		return nil
	default:
		return ErrBusy
	}
}

// Actions is what the main loop takes actions from.
func (m *Manager) Actions() <-chan Action { return m.actions }

// Apply carries out an action. It runs on the manager's goroutine.
func (m *Manager) Apply(ctx context.Context, a Action) {
	done := func(r Result) {
		if a.Done != nil {
			a.Done(r)
		}
	}
	defer m.publishSnapshot(ctx)

	if a.ID != "" {
		key := "bot:" + a.ID
		if m.J.Read().WasExecuted(key) {
			done(Result{Skipped: true})
			return
		}
		if err := m.J.Update(func(j *Journal) { j.MarkExecuted(key) }); err != nil {
			done(Result{Err: fmt.Errorf("could not write it down: %w", err)})
			return
		}
	}

	switch a.Kind {
	case ActMove:
		if !m.current {
			done(Result{Err: errors.New("this server has already handed over")})
			return
		}
		if a.Region != "" && !slices.Contains(m.Cfg.Regions, a.Region) {
			done(Result{Err: fmt.Errorf("region %q is not one this line moves between", a.Region)})
			return
		}
		m.log().Info("the operator asked for a move through the bot", "region", a.Region)
		m.pending = &MoveRequest{Trigger: TriggerCommand, Region: a.Region}
		done(Result{})

	case ActRestartNode:
		m.log().Info("the operator asked for the tunnel to be restarted")
		done(Result{Err: m.Host.RestartNode(ctx)})

	case ActPolicy:
		p := m.Cfg.Policy
		if a.AutoAge != nil {
			p.AutoAge = *a.AutoAge
		}
		if a.AutoBlock != nil {
			p.AutoBlock = *a.AutoBlock
		}
		if err := p.Normalize(); err != nil {
			done(Result{Err: err})
			return
		}
		before := m.Cfg.Policy
		m.Cfg.Policy = p
		if m.ConfigPath != "" {
			if err := m.Cfg.Save(m.ConfigPath); err != nil {
				m.Cfg.Policy = before
				done(Result{Err: fmt.Errorf("could not save the settings: %w", err)})
				return
			}
		}
		m.log().Info("the operator changed automatic moves", "auto_age", p.AutoAge, "auto_block", p.AutoBlock)
		m.statusDue = true
		done(Result{})

	case ActCheck:
		if m.Prober == nil {
			done(Result{Err: errors.New("this server has no external check")})
			return
		}
		target, control, err := m.Prober.Reach(ctx, m.Cfg.IP)
		if err != nil {
			done(Result{Err: err})
			return
		}
		done(Result{Check: &CheckResult{Target: target, Control: control, Round: Classify(target, control)}})

	default:
		done(Result{Err: fmt.Errorf("unknown action %q", a.Kind)})
	}
}

func (m *Manager) emit(e Event) {
	if m.OnEvent != nil {
		m.OnEvent(e)
	}
}

// checkCurrent finds out whether this server is the one the apps are told to
// use. A server that has published its replacement is not, whatever the
// places say a moment later.
func (m *Manager) checkCurrent(ctx context.Context, now time.Time) {
	if latest := m.latest(); latest != nil &&
		(latest.State == StatePublished || latest.State == StateRetiring || latest.State == StateRetired) {
		m.current, m.currentKnown = false, true
		return
	}
	every := currentCheckWaiting
	if m.current {
		every = currentCheckSettled
	}
	if m.currentKnown && now.Sub(m.lastCurrentCheck) < every {
		return
	}
	m.lastCurrentCheck = now
	body, _, err := m.Pub.Current(ctx)
	if err != nil {
		m.log().Debug("could not tell whether this server is the published one", "error", err)
		return
	}
	was := m.current
	m.current = endpointHost(body.Endpoint) == m.Cfg.IP
	m.currentKnown = true
	if m.current && !was {
		m.emit(Event{Kind: EventCurrent, Region: m.Cfg.Region, Generation: m.Cfg.Generation})
	}
}

// noteHealth tells the operator when the verdict changes.
func (m *Manager) noteHealth() {
	v := m.tracker.Verdict()
	if m.lastVerdict != "" && v != m.lastVerdict {
		m.emit(Event{Kind: EventHealth, Health: v, Why: m.why})
	}
	m.lastVerdict = v
}

// publishSnapshot records the state for the bot to read.
func (m *Manager) publishSnapshot(ctx context.Context) {
	now := m.now()
	j := m.J.Read()
	s := &Snapshot{
		At:           now,
		Generation:   m.Cfg.Generation,
		Region:       m.Cfg.Region,
		CreatedAt:    m.Cfg.CreatedTime(),
		Health:       m.tracker.Verdict(),
		Why:          m.why,
		AutoAge:      m.Cfg.Policy.AutoAge,
		AutoBlock:    m.Cfg.Policy.AutoBlock,
		CertDaysLeft: m.Cfg.CertDaysLeft(now),
		LastMove:     SummaryOf(j.Latest()),
		Watch:        m.watchSummary(),
		Regions:      append([]string(nil), m.Cfg.Regions...),
		Moving:       m.moving,
		Current:      m.current,
		CurrentKnown: m.currentKnown,
	}
	if next, ok := NextAutoMove(m.Cfg.Policy, m.Cfg.CreatedTime(), time.Duration(j.AgeJitterSec)*time.Second); ok {
		s.NextAutoMove = next
	}
	if st, err := m.Host.NodeStatus(ctx); err == nil {
		s.Node = Summarize(st)
	}
	m.snap.Store(s)
}
