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

	plan, refusal := s.Registry.Plan(req, now)
	if refusal != "" {
		s.log().Info("refused a device", "reason", refusal, "name", req.Name)
		return s.reply(proto.NewEnrollRefused(req.Nonce, refusal, now))
	}

	// The tunnel first, the record second. A device the tunnel accepts but the
	// record does not know is forgotten at the next restart and enrols again;
	// the other way round it would be told it is registered and fail.
	pub, err := ParseKey(plan.Device.PublicKey)
	if err != nil {
		return s.reply(proto.NewEnrollRefused(req.Nonce, proto.EnrollBadRequest, now))
	}
	if err := s.Peers.SetPeer(pub, plan.Device.Address); err != nil {
		s.log().Error("could not add the device to the tunnel", "error", err)
		return s.reply(proto.NewEnrollRefused(req.Nonce, proto.EnrollBusy, now))
	}
	if err := s.Registry.Commit(plan); err != nil {
		s.log().Error("could not save the device list", "error", err)
		if !plan.Existing {
			_ = s.Peers.RemovePeer(pub)
		}
		return s.reply(proto.NewEnrollRefused(req.Nonce, proto.EnrollBusy, now))
	}
	for _, old := range plan.Drop {
		k, err := ParseKey(old)
		if err != nil {
			continue
		}
		if err := s.Peers.RemovePeer(k); err != nil {
			s.log().Warn("could not remove a replaced key from the tunnel", "error", err)
		}
	}

	s.log().Info("enrolled a device", "name", plan.Device.Name, "address", plan.Device.Address,
		"known", plan.Existing, "devices", s.Registry.Len())
	return s.reply(proto.NewEnrolled(req, plan.Device.Address, now))
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
