package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"awgrotator/internal/node"
)

// The watch is a record, not a decision. Every few minutes the server looks at
// whether it can still get answers from Russia (the reverse probe), notes what
// its own clients are doing, and writes one line. Nothing reads the line back to
// decide anything. It exists so that when a server does stop working for the
// people it is meant for, there is a timeline to set against what actually
// happened, and the question "would this way of noticing have helped" can be
// answered from evidence instead of argued.

// WatchLogName is the file the samples are appended to, inside the manager's
// data directory.
const WatchLogName = "watch.jsonl"

// Tuning of the record.
const (
	// DefaultWatchEvery is how often the server looks.
	DefaultWatchEvery = 2 * time.Minute

	// watchLogMaxBytes is how big the log may get before it is set aside and a
	// new one started. Two files are kept, so about a month of lines.
	watchLogMaxBytes = 4 << 20

	// InheritedWatchBytes is how much of the end of the log a replacement is
	// given, so that the record continues across servers and survives the one
	// that was blocked being deleted.
	InheritedWatchBytes = 2 << 20

	// minStatusGap is the shortest time between two published statuses when the
	// reason is a change in what the watch sees.
	minStatusGap = 10 * time.Minute

	// maxWatchChanges is how many changes the published summary remembers.
	maxWatchChanges = 12
)

// Tally is how many of the checks of one group, over each protocol, were
// answered: {answered, asked}.
type Tally struct {
	TCP  [2]int `json:"tcp"`
	UDP  [2]int `json:"udp"`
	ICMP [2]int `json:"icmp"`
}

// PeerTally is what the server's own clients were doing at the time.
type PeerTally struct {
	Known int `json:"known"`
	Up    int `json:"up"` // handshook in the last three minutes

	// NewestS is how many seconds ago the most recent handshake was, or -1.
	NewestS int `json:"newest_s"`

	// RxBytes and TxBytes are the totals since the tunnel started.
	RxBytes int64 `json:"rx"`
	TxBytes int64 `json:"tx"`
}

// ExtSample is the answer of the Globalping check, put in the line that follows
// it, so that the two ways of looking can be set side by side.
type ExtSample struct {
	At      int64  `json:"at"`
	RU      [2]int `json:"ru"`
	Control [2]int `json:"ctl"`
	Round   string `json:"round"`
	Err     string `json:"err,omitempty"`
}

// Sample is one line of the log.
type Sample struct {
	At  int64 `json:"t"`
	Gen int   `json:"gen"`

	// Round is how the reverse probe came out: good, bad, inconclusive, or
	// unknown when it could not run.
	Round string `json:"round"`
	RU    Tally  `json:"ru"`
	Ctl   Tally  `json:"ctl"`
	RUms  int    `json:"ru_ms,omitempty"` // median TCP time to the Russian sites

	// Failed names the Russian targets that did not answer over TCP or UDP.
	Failed []string `json:"fail,omitempty"`

	Peers  *PeerTally `json:"peers,omitempty"`
	Ext    *ExtSample `json:"ext,omitempty"`
	Health string     `json:"health"`
	Err    string     `json:"err,omitempty"`
}

// WatchLog is the file of samples.
type WatchLog struct {
	Path     string
	MaxBytes int64
}

func (l *WatchLog) max() int64 {
	if l.MaxBytes > 0 {
		return l.MaxBytes
	}
	return watchLogMaxBytes
}

// Append writes one line. When the file would grow past its limit it is set
// aside as Path+".1", replacing an older one, and a fresh file is started.
func (l *WatchLog) Append(s Sample) error {
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(l.Path), 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(l.Path); err == nil && info.Size()+int64(len(line)) > l.max() {
		if err := os.Rename(l.Path, l.Path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Tail returns the last max bytes of the record, older file first, starting at a
// whole line. It returns nil when there is no log.
func (l *WatchLog) Tail(max int) ([]byte, error) {
	var all []byte
	for _, p := range []string{l.Path + ".1", l.Path} {
		raw, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		all = append(all, raw...)
	}
	if len(all) <= max {
		return all, nil
	}
	all = all[len(all)-max:]
	if i := bytes.IndexByte(all, '\n'); i >= 0 {
		all = all[i+1:]
	}
	return all, nil
}

// ParseSamples reads lines of a log, skipping any that are damaged: a log that
// was cut in the middle of a line by a crash is still worth reading.
func ParseSamples(raw []byte) []Sample {
	var out []Sample
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var s Sample
		if json.Unmarshal(line, &s) == nil && s.At != 0 {
			out = append(out, s)
		}
	}
	return out
}

// WatchChange is a time at which what the watch sees changed.
type WatchChange struct {
	At int64  `json:"at"`
	To string `json:"to"`

	// What is "reverse" for the server's own probe and "external" for the
	// Globalping check.
	What string `json:"what"`
}

// WatchSummary is the short account of the watch that goes in the published
// status. It holds counts and states and nothing that names a place.
type WatchSummary struct {
	Round   string        `json:"round"`
	Since   int64         `json:"since"`
	RU      string        `json:"ru"`
	Control string        `json:"control"`
	Changes []WatchChange `json:"changes,omitempty"`
}

// watchState is what the manager remembers between looks.
type watchState struct {
	last time.Time

	round string // of the last look, "" before the first
	since int64

	// extRound is how the Globalping check last came out, "" before the first.
	extRound string

	// ext waits to go into the next line.
	ext *ExtSample

	lastSample *Sample
	changes    []WatchChange

	// due says a change has happened that the published status should carry.
	due bool

	// logFailed stops the same write error being reported at every look.
	logFailed bool
}

func (w *watchState) change(at time.Time, what, to string) {
	w.changes = append(w.changes, WatchChange{At: at.Unix(), To: to, What: what})
	if len(w.changes) > maxWatchChanges {
		w.changes = w.changes[len(w.changes)-maxWatchChanges:]
	}
	w.due = true
}

func tally(r ReverseReport, group string) Tally {
	t := func(proto string) [2]int {
		c := r.Count(group, proto)
		return [2]int{c.Reached, c.Total}
	}
	return Tally{TCP: t(ProtoTCP), UDP: t(ProtoUDP), ICMP: t(ProtoICMP)}
}

func peerTally(s node.Status) *PeerTally {
	p := &PeerTally{Known: len(s.Peers), NewestS: -1}
	var newest time.Time
	for _, peer := range s.Peers {
		if peer.Connected(s.UpdatedAt) {
			p.Up++
		}
		if peer.LastHandshake.After(newest) {
			newest = peer.LastHandshake
		}
		p.RxBytes += peer.RxBytes
		p.TxBytes += peer.TxBytes
	}
	if !newest.IsZero() {
		p.NewestS = int(s.UpdatedAt.Sub(newest) / time.Second)
	}
	return p
}

// noteExternal remembers how the Globalping check came out, for the next line,
// and records a change when it differs from the last time.
func (m *Manager) noteExternal(now time.Time, target, control Reach, round Round, err error) {
	if m.Watch == nil {
		return
	}
	e := &ExtSample{
		At:      now.Unix(),
		RU:      [2]int{target.Reached, target.Total},
		Control: [2]int{control.Reached, control.Total},
		Round:   roundName(round),
	}
	if err != nil {
		e.Round = "unknown"
		e.Err = briefError(err)
	}
	m.watch.ext = e
	if m.watch.extRound != "" && m.watch.extRound != e.Round && e.Round != "unknown" {
		m.watch.change(now, "external", e.Round)
	}
	if e.Round != "unknown" {
		m.watch.extRound = e.Round
	}
}

func roundName(r Round) string {
	switch r {
	case Good:
		return "good"
	case Bad:
		return "bad"
	}
	return "inconclusive"
}

// maybeWatch takes a sample when one is due and writes it down.
func (m *Manager) maybeWatch(ctx context.Context, now time.Time) {
	if m.Watch == nil || m.Reverse == nil {
		return
	}
	if !m.watch.last.IsZero() && now.Sub(m.watch.last) < m.Every.Watch {
		return
	}
	m.watch.last = now

	s := Sample{At: now.Unix(), Gen: m.Cfg.Generation, Health: string(m.tracker.Verdict())}

	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	report, err := m.Reverse.Probe(pctx)
	cancel()
	if err != nil {
		s.Round = "unknown"
		s.Err = briefError(err)
	} else {
		s.Round = roundName(report.Round())
		s.RU, s.Ctl = tally(report, GroupRU), tally(report, GroupControl)
		s.RUms = report.MedianMs(GroupRU)
		s.Failed = report.Failed()
	}
	if st, err := m.Host.NodeStatus(ctx); err == nil {
		s.Peers = peerTally(st)
	}
	if m.watch.ext != nil {
		s.Ext, m.watch.ext = m.watch.ext, nil
	}

	if err := m.Watch.Append(s); err != nil {
		if !m.watch.logFailed {
			m.log().Warn("could not write the availability log", "error", err)
		}
		m.watch.logFailed = true
	} else {
		m.watch.logFailed = false
	}

	if s.Round != "unknown" {
		if m.watch.round != "" && m.watch.round != s.Round {
			m.watch.change(now, "reverse", s.Round)
			m.log().Info("what the watch sees has changed", "reverse", s.Round,
				"ru_tcp", s.RU.TCP, "ru_udp", s.RU.UDP, "control_tcp", s.Ctl.TCP)
		}
		if m.watch.round != s.Round {
			m.watch.since = now.Unix()
		}
		m.watch.round = s.Round
	}
	m.watch.lastSample = &s
}

// watchSummary is what the published status says about the watch, or nil before
// the first look.
func (m *Manager) watchSummary() *WatchSummary {
	s := m.watch.lastSample
	if m.Watch == nil || s == nil {
		return nil
	}
	return &WatchSummary{
		Round:   m.watch.round,
		Since:   m.watch.since,
		RU:      fmt.Sprintf("tcp %d/%d, udp %d/%d", s.RU.TCP[0], s.RU.TCP[1], s.RU.UDP[0], s.RU.UDP[1]),
		Control: fmt.Sprintf("tcp %d/%d, udp %d/%d", s.Ctl.TCP[0], s.Ctl.TCP[1], s.Ctl.UDP[0], s.Ctl.UDP[1]),
		Changes: append([]WatchChange(nil), m.watch.changes...),
	}
}
