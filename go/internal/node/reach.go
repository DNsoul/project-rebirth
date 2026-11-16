package node

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"awgrotator/internal/awg"
)

// The reach responder lets the outside check send UDP to the server and get an
// answer back.
//
// The tunnel itself cannot be asked: it stays silent to anything that is not a
// handshake from a known key, which is the point of it. So next to it, on the
// following port, the node answers one DNS question: an A query for a name
// derived from the server's private key. To everything else it says nothing,
// exactly like a closed port behind a firewall, so a scanner learns nothing and
// the server cannot be used to bounce traffic at anyone. The answer is barely
// bigger than the question and their number is capped.
//
// Globalping probes can send such a question from home networks in Russia
// (a DNS measurement aimed at this address and port). When they get the answer,
// UDP from that network reaches the server and comes back, which is what the
// tunnel needs; ping alone does not show it.

// ReachAnswer is the address in every answer, from the block set aside for
// documentation, so it can never be mistaken for anything real.
const ReachAnswer = "192.0.2.1"

const (
	// reachSuffix makes the derived label a well-formed name. The domain is
	// reserved for examples and never resolves to anything of ours.
	reachSuffix = ".example.com"

	// reachPerSecond caps the answers.
	reachPerSecond = 20
)

// ReachPort is the port the responder listens on: the one after the tunnel's.
// A port right next to it goes through the same port-range rules a network may
// have.
func ReachPort(listenPort int) int {
	if listenPort >= 65535 {
		return listenPort - 1
	}
	return listenPort + 1
}

// ReachName is the one name the responder answers to. It comes from the
// server's private key, so it stays the same across restarts, differs from
// server to server and cannot be worked out from outside.
func ReachName(key awg.Key) string {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("awg-rotator/reach/v1"))
	return hex.EncodeToString(mac.Sum(nil)[:8]) + reachSuffix
}

// ReachStatus is what the node says about its responder in its status.
type ReachStatus struct {
	Port     int    `json:"port"`
	Name     string `json:"name"`
	Answered uint64 `json:"answered"`
}

// ReachResponder answers the outside check.
type ReachResponder struct {
	Port int
	Name string

	answered atomic.Uint64

	// window and inWindow count answers per second.
	window   int64
	inWindow int
}

// Answered returns how many questions have been answered.
func (r *ReachResponder) Answered() uint64 { return r.answered.Load() }

// Serve answers on pc until the context ends.
func (r *ReachResponder) Serve(ctx context.Context, pc net.PacketConn) error {
	stop := context.AfterFunc(ctx, func() { _ = pc.Close() })
	defer stop()

	buf := make([]byte, 512)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue
		}
		reply, ok := answerReach(buf[:n], r.Name)
		if !ok || !r.allow(time.Now()) {
			continue
		}
		if _, err := pc.WriteTo(reply, from); err == nil {
			r.answered.Add(1)
		}
	}
}

// allow keeps the answers under reachPerSecond. Serve runs on one goroutine, so
// no lock is needed.
func (r *ReachResponder) allow(now time.Time) bool {
	sec := now.Unix()
	if sec != r.window {
		r.window, r.inWindow = sec, 0
	}
	if r.inWindow >= reachPerSecond {
		return false
	}
	r.inWindow++
	return true
}

// answerReach builds the answer to a question, or says there is none to give.
// Only a plain query with one question for name, of type A (or ANY) in class
// IN, gets one.
func answerReach(query []byte, name string) ([]byte, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response || h.OpCode != 0 {
		return nil, false
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 {
		return nil, false
	}
	q := qs[0]
	asked := strings.TrimSuffix(q.Name.String(), ".")
	if !strings.EqualFold(asked, name) || q.Class != dnsmessage.ClassINET {
		return nil, false
	}
	if q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeALL {
		return nil, false
	}

	b := dnsmessage.NewBuilder(make([]byte, 0, len(query)+16), dnsmessage.Header{
		ID:                 h.ID,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   h.RecursionDesired,
		RecursionAvailable: false,
	})
	b.EnableCompression()
	if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
		return nil, false
	}
	addr := net.ParseIP(ReachAnswer).To4()
	var a dnsmessage.AResource
	copy(a.A[:], addr)
	if b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 0}, a) != nil {
		return nil, false
	}
	out, err := b.Finish()
	if err != nil {
		return nil, false
	}
	return out, true
}
