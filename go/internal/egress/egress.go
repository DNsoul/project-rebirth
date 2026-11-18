// Package egress decides where the clients' traffic leaves the server from:
// straight from the server's own address, or through WARP.
//
// The way in and the way out are kept apart so that what a client app learns
// about the way out says nothing about the way in (see package warp). The
// direct path is never taken down: going through WARP is one routing rule on
// top of it, and taking that rule away is the whole of switching back. So the
// switch works however broken WARP is, and a node that dies takes the rule's
// route with its device, which leaves the clients on the direct path too.
//
// What is wanted comes from the manager (the operator's bot, awgctl) as a file;
// what is so is in the node's status. When WARP stops answering, the clients go
// out directly for a while and are told nothing: a few minutes of the server's
// own address is better than no internet. WARP is taken back once it has kept
// working for a while.
package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"awgrotator/internal/warp"
)

// Mode is where the traffic is asked to leave from.
type Mode string

// Modes.
const (
	Direct Mode = "direct"
	WARP   Mode = "warp"
)

// ParseMode reads a mode as the operator writes it; empty is Direct.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", Direct:
		return Direct, nil
	case WARP:
		return WARP, nil
	}
	return "", fmt.Errorf("unknown exit %q (direct or warp)", s)
}

// State is where it leaves from right now.
type State string

// States.
const (
	StateDirect   State = "direct"   // asked for, or nothing asked
	StateStarting State = "starting" // WARP asked for and being brought up
	StateWARP     State = "warp"     // through WARP
	StateFallback State = "fallback" // WARP asked for, not answering: direct for now
)

// Status is what the node reports.
type Status struct {
	Want      Mode      `json:"want"`
	State     State     `json:"state"`
	Since     time.Time `json:"since"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Handshake time.Time `json:"handshake,omitempty"`
}

// System is what is done on the machine.
type System interface {
	// Up brings the tunnel to WARP up for account a. Clients are not sent
	// through it yet.
	Up(ctx context.Context, a *warp.Account) error
	// Down takes it away, and the clients' rule with it.
	Down(ctx context.Context) error
	// Route sends the clients' traffic through the tunnel (on) or straight
	// out (off).
	Route(ctx context.Context, on bool) error
	// Probe asks the outside world, through the tunnel, how it was reached;
	// nil when the answer came through WARP.
	Probe(ctx context.Context) error
	// Handshake is when WARP last answered the tunnel.
	Handshake() (time.Time, bool)
}

// Defaults for the controller's timings.
const (
	DefaultProbeEvery  = 20 * time.Second // while the clients go through WARP
	DefaultRetryEvery  = 30 * time.Second // while they do not
	DefaultReturnAfter = 2 * time.Minute  // WARP working this long before it is taken back
	DefaultFailures    = 2                // failed probes in a row before falling back
)

// Controller moves the machine towards what is wanted. Step is called from one
// goroutine; Status from any.
type Controller struct {
	Sys System
	// Account returns the WARP account, registering one if there is none yet.
	Account func(ctx context.Context) (*warp.Account, error)
	Now     func() time.Time
	Log     *slog.Logger

	ProbeEvery, RetryEvery, ReturnAfter time.Duration
	Failures                            int

	mu sync.Mutex
	st Status

	// Touched by Step only.
	up, routed   bool
	everRouted   bool // since WARP was last asked for
	failures     int
	lastProbe    time.Time
	healthySince time.Time
	nextTry      time.Time
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.New(slog.DiscardHandler)
}

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Status returns how things stand.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.st
	if st.Want == "" {
		st.Want = Direct
	}
	if st.State == "" {
		st.State = StateDirect
	}
	return st
}

func (c *Controller) set(state State, err error, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if state != c.st.State {
		if c.st.State != "" {
			c.log().Info("the clients' way out changed", "from", c.st.State, "to", state, "error", errString(err))
		}
		c.st.State = state
		c.st.Since = now
	}
	c.st.Error = errString(err)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Step does what is due to get from how things are to want.
func (c *Controller) Step(ctx context.Context, want Mode) {
	now := c.now()
	if want != WARP {
		want = Direct
	}
	c.mu.Lock()
	changed := want != c.st.Want
	c.st.Want = want
	c.mu.Unlock()
	if changed {
		c.everRouted, c.failures = false, 0
		c.lastProbe, c.healthySince, c.nextTry = time.Time{}, time.Time{}, time.Time{}
	}

	if want == Direct {
		c.toDirect(ctx, now)
		return
	}

	if !c.up {
		if now.Before(c.nextTry) {
			return
		}
		if err := c.bringUp(ctx); err != nil {
			c.nextTry = now.Add(orDuration(c.RetryEvery, DefaultRetryEvery))
			c.set(StateFallback, err, now)
			return
		}
		c.up = true
		c.lastProbe = time.Time{}
		if s := c.Status().State; s == StateDirect {
			c.set(StateStarting, nil, now)
		}
	}

	every := orDuration(c.RetryEvery, DefaultRetryEvery)
	if c.routed {
		every = orDuration(c.ProbeEvery, DefaultProbeEvery)
	}
	if !c.lastProbe.IsZero() && now.Sub(c.lastProbe) < every {
		return
	}
	c.lastProbe = now
	err := c.Sys.Probe(ctx)
	c.mu.Lock()
	c.st.CheckedAt = now
	if hs, ok := c.Sys.Handshake(); ok {
		c.st.Handshake = hs
	}
	c.mu.Unlock()

	limit := c.Failures
	if limit <= 0 {
		limit = DefaultFailures
	}
	if err != nil {
		c.healthySince = time.Time{}
		c.failures++
		if c.failures < limit {
			return
		}
		if c.routed {
			if rerr := c.Sys.Route(ctx, false); rerr != nil {
				c.log().Error("could not take the clients off WARP", "error", rerr)
			}
			c.routed = false
			c.log().Warn("WARP does not answer; the clients go out directly for now", "error", err)
		}
		c.set(StateFallback, err, now)
		return
	}

	c.failures = 0
	if c.routed {
		c.set(StateWARP, nil, now)
		return
	}
	if c.healthySince.IsZero() {
		c.healthySince = now
	}
	// The first time after being asked, WARP is used as soon as it works. After
	// a fallback it has to keep working for a while first, so that a WARP that
	// comes and goes does not move the clients back and forth.
	if c.everRouted && now.Sub(c.healthySince) < orDuration(c.ReturnAfter, DefaultReturnAfter) {
		return
	}
	if err := c.Sys.Route(ctx, true); err != nil {
		c.set(StateFallback, err, now)
		return
	}
	c.routed, c.everRouted = true, true
	c.set(StateWARP, nil, now)
}

func (c *Controller) bringUp(ctx context.Context) error {
	if c.Account == nil {
		return errors.New("no way to get a WARP account")
	}
	a, err := c.Account(ctx)
	if err != nil {
		return err
	}
	return c.Sys.Up(ctx, a)
}

func (c *Controller) toDirect(ctx context.Context, now time.Time) {
	if c.routed {
		if err := c.Sys.Route(ctx, false); err != nil {
			c.log().Error("could not take the clients off WARP", "error", err)
			c.set(StateDirect, err, now)
			return // tried again at the next step
		}
		c.routed = false
	}
	if c.up {
		if err := c.Sys.Down(ctx); err != nil {
			c.log().Warn("could not take the tunnel to WARP down", "error", err)
		}
		c.up = false
	}
	c.set(StateDirect, nil, now)
}

// Close sends the clients out directly and takes WARP down, for a node that is
// stopping.
func (c *Controller) Close(ctx context.Context) {
	c.toDirect(ctx, c.now())
}

// DefaultWantPath is where the manager writes what it wants.
const DefaultWantPath = "/var/lib/awgnode/exit.json"

type wantFile struct {
	Want Mode `json:"want"`
}

// ReadWant reads what is wanted. A missing or unreadable file means Direct:
// nothing is sent through WARP that was not asked for.
func ReadWant(path string) (Mode, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Direct, nil
	}
	if err != nil {
		return Direct, err
	}
	var w wantFile
	if err := json.Unmarshal(raw, &w); err != nil {
		return Direct, fmt.Errorf("%s: %w", path, err)
	}
	m, err := ParseMode(string(w.Want))
	if err != nil {
		return Direct, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// MarshalWant is the file's content for mode m.
func MarshalWant(m Mode) []byte {
	raw, _ := json.Marshal(wantFile{Want: m})
	return append(raw, '\n')
}

// WriteWant writes what is wanted, for the node to pick up.
func WriteWant(path string, m Mode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, MarshalWant(m), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Accounts returns an Account function that keeps the account at path and
// registers one with reg when there is none.
func Accounts(path string, reg *warp.Client, log *slog.Logger) func(context.Context) (*warp.Account, error) {
	return func(ctx context.Context) (*warp.Account, error) {
		a, err := warp.Load(path)
		if err != nil {
			// A file that does not read is set aside, not trusted: a new account
			// costs nothing.
			aside := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
			_ = os.Rename(path, aside)
			if log != nil {
				log.Warn("the WARP account could not be read; registering a new one", "error", err, "kept_as", aside)
			}
			a = nil
		}
		if a != nil {
			return a, nil
		}
		a, err = reg.Register(ctx)
		if err != nil {
			return nil, err
		}
		if err := warp.Save(path, a); err != nil {
			return nil, fmt.Errorf("keep the WARP account: %w", err)
		}
		if log != nil {
			log.Info("registered with WARP", "endpoint", a.Endpoint)
		}
		return a, nil
	}
}
