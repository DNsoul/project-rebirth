package soak

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// verifyWithin is how long a new tunnel gets to carry its first request
	// before the attempt is written off.
	verifyWithin = 90 * time.Second

	// retryAfter is the pause between failed attempts to bring a tunnel up.
	retryAfter = 30 * time.Second

	// stallAfter is how many probes in a row must fail before a tunnel that is
	// still up is reported as stalled.
	stallAfter = 4
)

// armRunner runs one arm: it keeps a tunnel up with traffic through it, tries
// it from inside, tries its server from outside, and brings it down and up again
// on a schedule.
type armRunner struct {
	arm Arm
	cfg Config
	log *eventLog

	clock   func() time.Time
	sleepFn func(ctx context.Context, d time.Duration) error
	connect connector
	restart func(ctx context.Context) error

	// probe tries a tunnel from inside and says whether it carries traffic. It is
	// a field so that tests can decide the answer.
	probe func(ctx context.Context, tun Tunnel) bool

	started time.Time

	bytesIn, bytesOut atomic.Int64
	reqOK, reqErr     atomic.Int64
}

func newArmRunner(arm Arm, cfg Config, log *eventLog) *armRunner {
	a := &armRunner{arm: arm, cfg: cfg, log: log, clock: time.Now, sleepFn: sleepCtx}
	a.probe = a.probeTunnel
	switch arm.Kind {
	case KindAWG:
		a.connect = func(context.Context) (Tunnel, error) { return connectAWG(arm.AWG) }
	case KindSOCKS:
		a.connect = func(context.Context) (Tunnel, error) { return newSOCKS(arm.SOCKS) }
		a.restart = a.runRestart
	}
	return a
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *armRunner) sleep(ctx context.Context, d time.Duration) error { return a.sleepFn(ctx, d) }

func (a *armRunner) sleepUntil(ctx context.Context, t time.Time) error {
	if d := t.Sub(a.clock()); d > 0 {
		return a.sleep(ctx, d)
	}
	return nil
}

// ---------------------------------------------------------------- recording

func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// record logs the outcome of something tried. A success is quiet when quietOK
// is set: it goes to the file and not to the screen.
func (a *armRunner) record(ev, kind string, d time.Duration, err error, quietOK bool) {
	e := Event{At: a.clock(), Arm: a.arm.Name, Ev: ev, Kind: kind, MS: d.Milliseconds(), OK: boolp(err == nil)}
	if err != nil {
		e.Err = shorten(err.Error())
	}
	a.log.emit(e, err == nil && quietOK)
}

func (a *armRunner) recordURL(kind, target string, d time.Duration, err error) {
	shown := target
	if u, perr := url.Parse(target); perr == nil {
		shown = u.Host + u.Path
	}
	e := Event{At: a.clock(), Arm: a.arm.Name, Ev: "traffic", Kind: kind, MS: d.Milliseconds(),
		OK: boolp(err == nil), Data: map[string]any{"url": shown}}
	if err != nil {
		e.Err = shorten(err.Error())
	}
	a.log.emit(e, err == nil)
}

func (a *armRunner) recordData(ev, kind string, d time.Duration, data map[string]any) {
	a.log.emit(Event{At: a.clock(), Arm: a.arm.Name, Ev: ev, Kind: kind, MS: d.Milliseconds(),
		OK: boolp(true), Data: data}, true)
}

func (a *armRunner) note(ev string, err error, data map[string]any) {
	e := Event{At: a.clock(), Arm: a.arm.Name, Ev: ev, Data: data}
	if err != nil {
		e.OK = boolp(false)
		e.Err = shorten(err.Error())
	}
	a.log.emit(e, false)
}

// ----------------------------------------------------------------- the loop

// run keeps the arm going until ctx ends. firstReconnect is how long after the
// start the first scheduled teardown is.
func (a *armRunner) run(ctx context.Context, firstReconnect time.Duration) {
	a.started = a.clock()
	a.note("start", nil, map[string]any{
		"kind": a.arm.Kind, "server": a.arm.ServerIP, "reconnect_every": a.cfg.ReconnectEvery.D().String(),
	})

	var side sync.WaitGroup
	side.Add(2)
	go func() { defer side.Done(); a.directLoop(ctx) }()
	go func() { defer side.Done(); a.summaryLoop(ctx) }()

	next := a.started.Add(firstReconnect)
	first := true
	for ctx.Err() == nil {
		tun, ok := a.bringUp(ctx, first)
		if !ok {
			break
		}
		first = false

		gen, cancel := context.WithCancel(ctx)
		var work sync.WaitGroup
		work.Add(2)
		go func() { defer work.Done(); a.trafficLoop(gen, tun) }()
		go func() { defer work.Done(); a.probeLoop(gen, tun) }()

		_ = a.sleepUntil(ctx, next)
		cancel()
		work.Wait()
		tun.Close()
		if ctx.Err() == nil {
			a.note("teardown", nil, map[string]any{"after": a.clock().Sub(a.started).Round(time.Second).String()})
		}
		next = next.Add(a.cfg.ReconnectEvery.D())
	}
	side.Wait()
	a.note("stop", nil, map[string]any{
		"in_bytes": a.bytesIn.Load(), "out_bytes": a.bytesOut.Load(),
	})
}

// bringUp brings a tunnel up and checks that it carries traffic, trying again
// for as long as it takes. How long a reconnection takes, and whether it ever
// succeeds, is the most telling thing this experiment records.
func (a *armRunner) bringUp(ctx context.Context, first bool) (Tunnel, bool) {
	began := a.clock()
	for attempt := 1; ctx.Err() == nil; attempt++ {
		tun, err := a.open(ctx, !first)
		if err == nil {
			ev := "reconnect"
			if first {
				ev = "up"
			}
			a.log.emit(Event{At: a.clock(), Arm: a.arm.Name, Ev: ev, OK: boolp(true),
				MS:   a.clock().Sub(began).Milliseconds(),
				Data: map[string]any{"attempt": attempt, "seconds": int(a.clock().Sub(began).Seconds())}}, false)
			return tun, true
		}
		if ctx.Err() != nil {
			return nil, false
		}
		a.note("connect", err, map[string]any{
			"attempt": attempt, "down_for": a.clock().Sub(began).Round(time.Second).String(),
		})
		if a.sleep(ctx, retryAfter) != nil {
			return nil, false
		}
	}
	return nil, false
}

// open makes one attempt: it restarts the tunnel where something else runs it,
// connects, and waits for the first request to get through.
func (a *armRunner) open(ctx context.Context, restart bool) (Tunnel, error) {
	if restart && a.restart != nil {
		if err := a.restart(ctx); err != nil {
			return nil, fmt.Errorf("restart: %w", err)
		}
	}
	raw, err := a.connect(ctx)
	if err != nil {
		return nil, err
	}
	tun := counted{Tunnel: raw, in: &a.bytesIn, out: &a.bytesOut}

	// The wait is measured on the arm's clock, and the context is only a
	// backstop, so that the whole thing can be run against a fake clock.
	deadline := a.clock().Add(verifyWithin)
	vctx, cancel := context.WithTimeout(ctx, verifyWithin+10*time.Second)
	defer cancel()
	for {
		if a.probe(vctx, tun) {
			return tun, nil
		}
		if a.clock().After(deadline) {
			break
		}
		if a.sleep(vctx, 3*time.Second) != nil {
			break
		}
	}
	tun.Close()
	return nil, fmt.Errorf("nothing came through the new tunnel in %s", verifyWithin)
}

// runRestart runs the command that restarts a tunnel run elsewhere.
func (a *armRunner) runRestart(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(rctx, a.arm.Restart[0], a.arm.Restart[1:]...).CombinedOutput()
	data := map[string]any{"output": shorten(string(out))}
	a.note("restart", err, data)
	return err
}

// probeLoop tries the tunnel from inside at a steady interval, and says when it
// stops answering although it is still up, and when it recovers.
func (a *armRunner) probeLoop(ctx context.Context, tun Tunnel) {
	fails, stalled := 0, false
	for {
		if a.sleep(ctx, a.cfg.ProbeEvery.D()) != nil {
			return
		}
		if a.probe(ctx, tun) {
			if stalled {
				a.note("recovered", nil, nil)
			}
			fails, stalled = 0, false
			continue
		}
		if ctx.Err() != nil {
			return
		}
		fails++
		if fails == stallAfter && !stalled {
			stalled = true
			a.note("stalled", fmt.Errorf("%d probes in a row failed on a tunnel that is up", fails), nil)
		}
	}
}

// summaryLoop writes the volume carried every minute, and shows it every ten.
func (a *armRunner) summaryLoop(ctx context.Context) {
	var lastIn, lastOut, lastOK, lastErr int64
	for tick := 1; ; tick++ {
		if a.sleep(ctx, time.Minute) != nil {
			return
		}
		in, out := a.bytesIn.Load(), a.bytesOut.Load()
		ok, bad := a.reqOK.Load(), a.reqErr.Load()
		e := Event{At: a.clock(), Arm: a.arm.Name, Ev: "volume", OK: boolp(true), Data: map[string]any{
			"in": in - lastIn, "out": out - lastOut, "requests_ok": ok - lastOK, "requests_failed": bad - lastErr,
			"total_mb": (in + out) / 1_000_000,
		}}
		a.log.emit(e, tick%10 != 0)
		lastIn, lastOut, lastOK, lastErr = in, out, ok, bad
	}
}

// Run runs every arm in the configuration until ctx ends. Progress is written
// to console and to the logs in the configured directory.
func Run(ctx context.Context, cfg Config, console io.Writer) error {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	log, err := newEventLog(cfg.LogDir, console)
	if err != nil {
		return err
	}
	defer log.close()

	var wg sync.WaitGroup
	for i, arm := range cfg.Arms {
		a := newArmRunner(arm, cfg, log)
		offset := cfg.ReconnectEvery.D() + time.Duration(i)*cfg.Stagger.D()
		wg.Add(1)
		go func() { defer wg.Done(); a.run(ctx, offset) }()
	}
	wg.Wait()
	return nil
}
