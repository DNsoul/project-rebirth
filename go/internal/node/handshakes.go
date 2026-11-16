package node

import (
	"fmt"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/device"
)

// The handshake watch notices clients that knock and never get in.
//
// A client opens a session by sending a handshake initiation; the server
// answers it, and the session counts as complete on the server only when the
// client's next packet arrives under the new keys. So "the server answered, the
// client never followed up" means the answer did not reach the client, or the
// client's packets stopped reaching the server right after. Seen from the
// server, that is the plainest sign of a path being cut while the address is
// still reachable in one direction: on 2026-11-07 a blocked server received
// initiations from Russia, answered every one, and never completed a handshake.
//
// The tunnel library reports each initiation it answers through its log, which
// is where this learns of them. The library names the peer by an abbreviated
// key, so peers are matched on that abbreviation.

// Tuning of the watch.
const (
	// knockGrace is how long a client may take to follow its handshake up
	// before it counts as stuck. A client whose answers are lost retries every
	// five seconds; one whose first packet after the handshake was merely lost
	// (it happens with the protocol's padding on) sends the next within its
	// keepalive interval of 25 seconds.
	knockGrace = 45 * time.Second

	// knockRecent is how recent the last initiation must be for a stuck client
	// to still count: one that gave up long ago says nothing about now.
	knockRecent = HandshakeFresh

	// knockForget is when a client that stopped knocking is forgotten.
	knockForget = time.Hour
)

// HandshakeStats is what the node says about handshakes in its status.
type HandshakeStats struct {
	// Answered is how many initiations the server has answered since the
	// tunnel started.
	Answered uint64 `json:"answered"`

	// Stuck is how many clients are knocking right now without ever completing
	// a handshake.
	Stuck int `json:"stuck"`
}

type knock struct {
	pending      int       // initiations answered since the last completed handshake
	firstPending time.Time // the first of those
	last         time.Time // the latest initiation
}

// HandshakeWatch counts answered initiations per client.
type HandshakeWatch struct {
	mu       sync.Mutex
	peers    map[string]*knock // by abbreviated key, "ABCD…WXYZ"
	answered uint64
}

// initiationAnswered is the library's log line for an initiation it accepted
// and is about to answer (device/receive.go). A test holds this to the real
// library, so an update that rewords it fails there rather than silently.
const initiationAnswered = "%v - Received handshake initiation"

// Logger returns a log for the tunnel library that hands errors to errorf, as
// before, and picks the answered initiations out of its other chatter. The
// rest of that chatter is dropped, as it was with the library's own logger at
// the error level.
func (w *HandshakeWatch) Logger(errorf func(format string, args ...any)) *device.Logger {
	return &device.Logger{
		Verbosef: func(format string, args ...any) {
			if format == initiationAnswered && len(args) > 0 {
				w.Initiation(fmt.Sprint(args[0]), time.Now())
			}
		},
		Errorf: errorf,
	}
}

// Initiation records that the server answered an initiation from the peer the
// library calls who, which looks like "peer(ABCD…WXYZ)".
func (w *HandshakeWatch) Initiation(who string, at time.Time) {
	id := trimPeerName(who)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.peers == nil {
		w.peers = map[string]*knock{}
	}
	k := w.peers[id]
	if k == nil {
		k = &knock{}
		w.peers[id] = k
	}
	if k.pending == 0 {
		k.firstPending = at
	}
	k.pending++
	k.last = at
	w.answered++
}

// Observe sets each peer's HandshakeTries from what it knows and returns the
// totals. It reads completed handshakes from the peers' LastHandshake: a
// handshake completed after the last initiation clears that client.
func (w *HandshakeWatch) Observe(peers []PeerStatus, now time.Time) HandshakeStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := HandshakeStats{Answered: w.answered}
	seen := map[string]bool{}
	for i := range peers {
		p := &peers[i]
		id := abbreviateKey(p.PublicKey)
		seen[id] = true
		k := w.peers[id]
		if k == nil || k.pending == 0 {
			continue
		}
		// The library reports handshake times in whole seconds, and the
		// follow-up comes a fraction of a second after the initiation.
		if !p.LastHandshake.IsZero() && !p.LastHandshake.Before(k.last.Truncate(time.Second)) {
			k.pending = 0
			continue
		}
		if now.Sub(k.firstPending) >= knockGrace && now.Sub(k.last) <= knockRecent {
			p.HandshakeTries = k.pending
			st.Stuck++
		}
	}
	for id, k := range w.peers {
		if !seen[id] && now.Sub(k.last) > knockForget {
			delete(w.peers, id)
		}
	}
	return st
}

// abbreviateKey shortens a base64 key the way the tunnel library does in its
// log: the first four and the last four characters before the padding.
func abbreviateKey(b64 string) string {
	if len(b64) < 43 {
		return b64
	}
	return b64[0:4] + "…" + b64[39:43]
}

// trimPeerName turns "peer(ABCD…WXYZ)" into "ABCD…WXYZ".
func trimPeerName(s string) string {
	const prefix, suffix = "peer(", ")"
	if len(s) > len(prefix)+len(suffix) && s[:len(prefix)] == prefix && s[len(s)-1:] == suffix {
		return s[len(prefix) : len(s)-1]
	}
	return s
}
