package enroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/proto"
)

// ListenPort is the UDP port, inside the tunnel, that enrollment is answered on.
// It is reachable only through the enrollment peer, so nothing about it shows
// from outside.
const ListenPort = 7843

// PeerSink is where the server puts the peers it registers: the live tunnel.
type PeerSink interface {
	// SetPeer starts accepting the key and routes the address to it. Calling
	// it again for a key that is already there is harmless.
	SetPeer(pub awg.Key, allowedIP string) error

	// RemovePeer stops accepting the key.
	RemovePeer(pub awg.Key) error
}

// Server answers enrollment requests.
type Server struct {
	Registry   *Registry
	Peers      PeerSink
	ContentKey []byte

	// From is the only source address answered: the enrollment peer's tunnel
	// address. Everything else is ignored without a reply.
	From netip.Addr

	// Access holds the waiting list and the refusals. Without it nobody is
	// refused and nothing waits. With it, refused devices are always turned
	// away, and with Approval set a device the operator has not approved waits.
	Access   *Access
	Approval bool

	// Preapproved are keys accepted without the operator, base64url: the key
	// the replacing server's own check registers with. It is secret and made
	// afresh for every server, which is what lets it skip the queue.
	Preapproved []string

	Log *slog.Logger
	Now func() time.Time

	// Burst and Every shape the request limit. The enrollment peer is shared by
	// whoever is enrolling, so the limit is one for all of them: it keeps a
	// flood from using up the registry's disk writes and the tunnel's peers.
	Burst int
	Every time.Duration

	mu     sync.Mutex // one enrollment at a time
	bucket bucket
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Handle answers one datagram. A nil result means "say nothing": the sender
// is not the enrollment peer, or did not show the content key, and gets no
// reply that would confirm anything is listening.
func (s *Server) Handle(from netip.Addr, datagram []byte) []byte {
	if from != s.From {
		return nil
	}
	now := s.now()
	req, err := proto.OpenEnrollRequest(s.ContentKey, string(datagram), now)
	if err != nil {
		s.log().Debug("ignored an enrollment that did not check out", "error", err)
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.allow(now) {
		return s.reply(proto.NewEnrollRefused(req.Nonce, proto.EnrollBusy, now))
	}

	if reason := s.gate(req, now); reason != "" {
		return s.reply(proto.NewEnrollRefused(req.Nonce, reason, now))
	}

	_, known := s.Registry.Lookup(req.DeviceID)
	d, err := s.admit(req.DeviceID, req.PublicKey, req.Name, now)
	var refused *RefusedError
	switch {
	case errors.As(err, &refused):
		s.log().Info("refused a device", "reason", refused.Reason, "name", req.Name)
		return s.reply(proto.NewEnrollRefused(req.Nonce, refused.Reason, now))
	case err != nil:
		s.log().Error("could not register a device", "error", err)
		return s.reply(proto.NewEnrollRefused(req.Nonce, proto.EnrollBusy, now))
	}

	s.log().Info("enrolled a device", "name", d.Name, "address", d.Address,
		"known", known, "devices", s.Registry.Len())
	return s.reply(proto.NewEnrolled(req, d.Address, now))
}

// gate decides whether a request may go on to be registered. An empty answer
// lets it through; anything else is the reason to send back.
//
// A device approved under the same key, or a preapproved key, goes through as
// before. A refused device, or a refused key, is turned away. On a server that
// asks the operator, everything else waits: a new device, and an approved one
// that comes with a different key, since that is either a reinstalled app or
// somebody claiming to be that device, and only the operator can tell which.
func (s *Server) gate(req *proto.EnrollRequest, now time.Time) string {
	if s.Access == nil || s.preapproved(req.PublicKey) {
		return ""
	}
	// Refusals first: a refusal that somehow sits next to an approval must
	// still win, or a device that was cut off could carry on with its old key.
	if s.Access.Refused(req) {
		s.log().Info("turned away a refused device", "name", req.Name)
		return proto.EnrollDenied
	}
	known, ok := s.Registry.Lookup(req.DeviceID)
	if ok && known.PublicKey == req.PublicKey {
		return ""
	}
	if !s.Approval {
		return ""
	}
	r, fresh, err := s.Access.Ask(req, ok, now)
	switch {
	case errors.Is(err, ErrQueueFull):
		s.log().Warn("the waiting list is full; a request was not taken", "name", req.Name)
		return proto.EnrollBusy
	case err != nil:
		s.log().Error("could not save the waiting list", "error", err)
		return proto.EnrollBusy
	}
	if fresh {
		s.log().Info("a device is waiting for approval", "name", r.Name, "key_change", r.KeyChange)
	}
	return proto.EnrollPending
}

func (s *Server) preapproved(pub string) bool {
	for _, k := range s.Preapproved {
		if k == pub {
			return true
		}
	}
	return false
}

func (s *Server) reply(r *proto.EnrollResponse) []byte {
	blob, err := r.Seal(s.ContentKey)
	if err != nil {
		s.log().Error("could not seal a reply", "error", err)
		return nil
	}
	return []byte(blob)
}

// ParseKey reads a tunnel key as it appears in the messages and the registry.
func ParseKey(s string) (awg.Key, error) {
	raw, err := proto.Decode(s, proto.TunnelKeySize)
	if err != nil {
		return awg.Key{}, err
	}
	var k awg.Key
	copy(k[:], raw)
	return k, nil
}

// KeyString is ParseKey's reverse.
func KeyString(k awg.Key) string { return proto.Encode(k[:]) }

// Serve reads datagrams from pc until the context ends, answering each. It
// closes pc on the way out.
func (s *Server) Serve(ctx context.Context, pc net.PacketConn) error {
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()

	buf := make([]byte, 2048)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		ap, ok := addrPort(addr)
		if !ok {
			continue
		}
		if out := s.Handle(ap.Addr(), buf[:n]); out != nil {
			if _, err := pc.WriteTo(out, addr); err != nil && ctx.Err() == nil {
				s.log().Warn("could not send a reply", "error", err)
			}
		}
	}
}

func addrPort(a net.Addr) (netip.AddrPort, bool) {
	if u, ok := a.(*net.UDPAddr); ok {
		ap := u.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
	}
	ap, err := netip.ParseAddrPort(a.String())
	return ap, err == nil
}

// bucket is a token bucket.
type bucket struct {
	tokens float64
	last   time.Time
	init   bool
}

func (s *Server) allow(now time.Time) bool {
	burst := s.Burst
	if burst <= 0 {
		burst = 20
	}
	every := s.Every
	if every <= 0 {
		every = 2 * time.Second
	}
	b := &s.bucket
	if !b.init {
		b.tokens, b.last, b.init = float64(burst), now, true
	}
	b.tokens = min(float64(burst), b.tokens+now.Sub(b.last).Seconds()/every.Seconds())
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
