package splitdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Mapper installs and removes the translation from a stand-in address to the
// real one. The production implementation drives nftables; tests use a fake.
type Mapper interface {
	Add(fake, real netip.Addr) error
	Delete(fake netip.Addr) error
}

// Defaults for the knobs in Config.
const (
	defaultTunnelTTL    = 60
	defaultMaxDirectTTL = 300
	defaultMaxStandIns  = 4
	cacheMin            = 5 * time.Second
	cacheMax            = 60 * time.Second
	cacheEntries        = 20000
)

// DNS record types the server treats specially. dnsmessage has no constants
// for them.
const (
	typeSVCB  dnsmessage.Type = 64
	typeHTTPS dnsmessage.Type = 65
)

// canaryNames are the names browsers and operating systems ask in order to
// decide whether to use their own encrypted DNS or relay. An NXDOMAIN answer
// is the documented way to tell them not to, which keeps their queries going
// through this resolver instead of around it.
var canaryNames = map[string]bool{
	"use-application-dns.net": true, // Firefox: do not enable DNS over HTTPS
	"mask.icloud.com":         true, // Apple: do not use Private Relay
	"mask-h2.icloud.com":      true,
}

// Config is what a Server is built from.
type Config struct {
	Decider *Decider
	Pool    *Pool
	Mapper  Mapper

	// Primary is asked with the local client subnet. Its answer is what a
	// client gets for a direct site, so it should show the addresses a local
	// client would be given.
	Primary Resolver

	// Secondary is asked without one. Together the two views widen what is
	// known about a name: a site that hands out a foreign address to anyone
	// but a local client is still caught.
	Secondary Resolver

	Log *slog.Logger

	// TunnelTTL is how long clients may keep a stand-in address.
	TunnelTTL uint32

	// MaxDirectTTL caps the TTL passed on for real addresses.
	MaxDirectTTL uint32

	// MaxStandIns caps how many addresses a tunnelled answer carries.
	MaxStandIns int

	// Now is the clock; tests replace it.
	Now func() time.Time
}

// Stats counts what the server has done. Names are never recorded: the
// resolver sees every site the user visits, and a counter is all that is
// needed to tell whether it is working.
type Stats struct {
	Queries  uint64 `json:"queries"`
	Direct   uint64 `json:"direct"`
	Tunnel   uint64 `json:"tunnel"`
	Filtered uint64 `json:"filtered"`
	Failures uint64 `json:"failures"`
	PoolSize int    `json:"pool_size"`
}

// Server answers DNS queries from tunnel clients.
type Server struct {
	cfg   Config
	cache *resolutionCache

	// mapMu serialises installing stand-ins, so that a second query for the
	// same real address cannot hand the stand-in out before the first has
	// finished putting the translation in place.
	mapMu sync.Mutex

	queries, direct, tunnel, filtered, failures atomic.Uint64
}

// NewServer checks the configuration and builds a server.
func NewServer(cfg Config) (*Server, error) {
	switch {
	case cfg.Decider == nil:
		return nil, errors.New("splitdns: no decider")
	case cfg.Pool == nil:
		return nil, errors.New("splitdns: no pool")
	case cfg.Mapper == nil:
		return nil, errors.New("splitdns: no mapper")
	case cfg.Primary == nil:
		return nil, errors.New("splitdns: no primary resolver")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.TunnelTTL == 0 {
		cfg.TunnelTTL = defaultTunnelTTL
	}
	if cfg.MaxDirectTTL == 0 {
		cfg.MaxDirectTTL = defaultMaxDirectTTL
	}
	if cfg.MaxStandIns <= 0 {
		cfg.MaxStandIns = defaultMaxStandIns
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Server{cfg: cfg, cache: newResolutionCache(cfg.Now)}, nil
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	return Stats{
		Queries:  s.queries.Load(),
		Direct:   s.direct.Load(),
		Tunnel:   s.tunnel.Load(),
		Filtered: s.filtered.Load(),
		Failures: s.failures.Load(),
		PoolSize: s.cfg.Pool.Len(),
	}
}

// Handle answers one query. It returns nil when the packet is not something
// that deserves an answer, which for DNS means dropping it.
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	var req dnsmessage.Message
	if err := req.Unpack(raw); err != nil {
		return nil
	}
	// Never answer a response: two such servers would talk to each other
	// forever.
	if req.Response {
		return nil
	}
	s.queries.Add(1)

	switch {
	case req.OpCode != 0:
		return s.reply(&req, dnsmessage.RCodeNotImplemented, nil)
	case len(req.Questions) != 1:
		return s.reply(&req, dnsmessage.RCodeFormatError, nil)
	case req.Questions[0].Class != dnsmessage.ClassINET:
		return s.reply(&req, dnsmessage.RCodeRefused, nil)
	}

	q := req.Questions[0]
	name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))

	if canaryNames[name] && isAddressType(q.Type) {
		return s.reply(&req, dnsmessage.RCodeNameError, nil)
	}

	switch q.Type {
	case dnsmessage.TypeA:
		return s.answerA(ctx, &req, q, name)
	case dnsmessage.TypeAAAA, typeHTTPS, typeSVCB:
		return s.answerAddressLike(ctx, &req, q, name)
	default:
		return s.forward(ctx, &req, q, name)
	}
}

func isAddressType(t dnsmessage.Type) bool {
	return t == dnsmessage.TypeA || t == dnsmessage.TypeAAAA || t == typeHTTPS || t == typeSVCB
}

func (s *Server) answerA(ctx context.Context, req *dnsmessage.Message, q dnsmessage.Question, name string) []byte {
	res, err := s.resolve(ctx, name)
	if err != nil {
		return s.fail(req, name, err)
	}
	switch {
	case res.rcode == dnsmessage.RCodeNameError:
		return s.reply(req, dnsmessage.RCodeNameError, nil)
	case len(res.addrs) == 0:
		// The name exists but has no usable address: say so, and do not
		// invent one.
		return s.reply(req, dnsmessage.RCodeSuccess, nil)
	}

	if res.decision.Verdict == Direct {
		s.direct.Add(1)
		s.cfg.Log.Debug("answered", "name", name, "verdict", "direct", "reason", res.decision.Reason)
		ttl := min(res.ttl, s.cfg.MaxDirectTTL)
		return s.reply(req, dnsmessage.RCodeSuccess, aRecords(q.Name, res.direct, ttl))
	}

	standIns, err := s.standIns(res.addrs)
	if err != nil {
		return s.fail(req, name, err)
	}
	s.tunnel.Add(1)
	s.cfg.Log.Debug("answered", "name", name, "verdict", "tunnel", "reason", res.decision.Reason)
	return s.reply(req, dnsmessage.RCodeSuccess, aRecords(q.Name, standIns, min(res.ttl, s.cfg.TunnelTTL)))
}

// answerAddressLike handles the record types that can carry an address of
// their own: AAAA, and the HTTPS and SVCB hints. For a tunnelled name they are
// answered with nothing, because an application that found a real address
// there would connect to it directly and skip the stand-in. For a direct name
// they are passed through untouched.
func (s *Server) answerAddressLike(ctx context.Context, req *dnsmessage.Message, q dnsmessage.Question, name string) []byte {
	res, err := s.resolve(ctx, name)
	if err != nil {
		return s.fail(req, name, err)
	}
	switch {
	case res.rcode == dnsmessage.RCodeNameError:
		return s.reply(req, dnsmessage.RCodeNameError, nil)
	case res.decision.Verdict == Tunnel:
		return s.reply(req, dnsmessage.RCodeSuccess, nil)
	}
	return s.forward(ctx, req, q, name)
}

// forward passes a query upstream and relays the answer unchanged.
func (s *Server) forward(ctx context.Context, req *dnsmessage.Message, q dnsmessage.Question, name string) []byte {
	msg, err := s.cfg.Primary.Query(ctx, name, q.Type)
	if err != nil {
		return s.fail(req, name, err)
	}
	return s.reply(req, msg.RCode, msg.Answers)
}

// resolution is what is known about a name after asking upstream.
type resolution struct {
	decision Decision
	rcode    dnsmessage.RCode

	// addrs is every usable address from both views, for the tunnel.
	addrs []netip.Addr

	// direct is what a direct client is given: the local view when there is
	// one.
	direct []netip.Addr

	ttl     uint32
	expires time.Time
}

// resolve asks both upstreams, merges what they say and applies the policy.
func (s *Server) resolve(ctx context.Context, name string) (*resolution, error) {
	if res := s.cache.get(name); res != nil {
		return res, nil
	}

	type answer struct {
		msg *dnsmessage.Message
		err error
	}
	var primary, secondary answer
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		primary.msg, primary.err = s.cfg.Primary.Query(ctx, name, dnsmessage.TypeA)
	}()
	if s.cfg.Secondary != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			secondary.msg, secondary.err = s.cfg.Secondary.Query(ctx, name, dnsmessage.TypeA)
		}()
	} else {
		secondary.err = errors.New("not configured")
	}
	wg.Wait()

	if primary.err != nil && secondary.err != nil {
		return nil, fmt.Errorf("every upstream failed: %w", primary.err)
	}

	res := &resolution{rcode: dnsmessage.RCodeSuccess}

	// A name that does not exist stays non-existent however it is routed.
	for _, a := range []answer{primary, secondary} {
		if a.err == nil && a.msg.RCode == dnsmessage.RCodeNameError {
			res.rcode = dnsmessage.RCodeNameError
			res.decision = Decision{Tunnel, "name does not exist"}
			res.ttl = uint32(cacheMin / time.Second)
			return s.remember(name, res), nil
		}
	}

	// A view that answered with an error code is treated as having failed.
	view := func(a answer) (addrs []netip.Addr, ttl uint32, ok bool) {
		if a.err != nil || a.msg.RCode != dnsmessage.RCodeSuccess {
			return nil, 0, false
		}
		addrs, ttl = addrsOf(a.msg)
		return addrs, ttl, true
	}
	pAddrs, pTTL, pOK := view(primary)
	sAddrs, sTTL, sOK := view(secondary)
	if !pOK && !sOK {
		return nil, errors.New("every upstream answered with an error")
	}

	// Drop addresses that must never become a translation target. If that
	// removes everything the name resolves to, the client gets no answer
	// rather than a stand-in for a service on the server itself.
	rawCount := len(pAddrs) + len(sAddrs)
	pAddrs, sAddrs = FilterPublic(pAddrs), FilterPublic(sAddrs)
	if dropped := rawCount - len(pAddrs) - len(sAddrs); dropped > 0 {
		s.filtered.Add(uint64(dropped))
	}

	res.addrs = unique(pAddrs, sAddrs)
	res.direct = pAddrs
	if len(res.direct) == 0 {
		res.direct = sAddrs
	}
	res.ttl = smallestTTL(pTTL, sTTL)
	res.decision = s.cfg.Decider.Decide(name, res.addrs)
	return s.remember(name, res), nil
}

func (s *Server) remember(name string, res *resolution) *resolution {
	hold := time.Duration(res.ttl) * time.Second
	hold = min(max(hold, cacheMin), cacheMax)
	res.expires = s.cfg.Now().Add(hold)
	s.cache.put(name, res)
	return res
}

// standIns returns a stand-in address for each of the real ones, up to the
// configured limit, installing translations as needed.
func (s *Server) standIns(real []netip.Addr) ([]netip.Addr, error) {
	s.mapMu.Lock()
	defer s.mapMu.Unlock()

	limit := min(len(real), s.cfg.MaxStandIns)
	out := make([]netip.Addr, 0, limit)
	for _, r := range real[:limit] {
		as, err := s.cfg.Pool.Assign(r)
		if err != nil {
			return nil, err
		}
		if as.Evicted != nil {
			// The recycled address must stop pointing at its old target
			// before it is pointed at the new one.
			if err := s.cfg.Mapper.Delete(as.Evicted.Fake); err != nil {
				return nil, fmt.Errorf("remove recycled translation: %w", err)
			}
		}
		if as.Created {
			if err := s.cfg.Mapper.Add(as.Fake, r); err != nil {
				// Not installed, so it must not be handed out, and must not
				// be remembered as if it were.
				s.cfg.Pool.Release(as.Fake)
				return nil, fmt.Errorf("install translation: %w", err)
			}
		}
		out = append(out, as.Fake)
	}
	return out, nil
}

func (s *Server) fail(req *dnsmessage.Message, name string, err error) []byte {
	s.failures.Add(1)
	// The name is deliberately left out of the message: at the default log
	// level this resolver writes nothing that says which sites were visited.
	s.cfg.Log.Warn("query failed", "error", err)
	s.cfg.Log.Debug("query failed", "name", name)
	return s.reply(req, dnsmessage.RCodeServerFailure, nil)
}

func (s *Server) reply(req *dnsmessage.Message, rcode dnsmessage.RCode, answers []dnsmessage.Resource) []byte {
	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 req.ID,
			Response:           true,
			RecursionDesired:   req.RecursionDesired,
			RecursionAvailable: true,
			RCode:              rcode,
		},
		Questions: req.Questions,
		Answers:   answers,
	}
	out, err := resp.Pack()
	if err != nil {
		s.cfg.Log.Error("could not build a reply", "error", err)
		return nil
	}
	return out
}

func aRecords(name dnsmessage.Name, addrs []netip.Addr, ttl uint32) []dnsmessage.Resource {
	out := make([]dnsmessage.Resource, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{
				Name:  name,
				Type:  dnsmessage.TypeA,
				Class: dnsmessage.ClassINET,
				TTL:   ttl,
			},
			Body: &dnsmessage.AResource{A: a.As4()},
		})
	}
	return out
}

// unique merges address lists, keeping the order of first appearance.
func unique(lists ...[]netip.Addr) []netip.Addr {
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	for _, l := range lists {
		for _, a := range l {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// smallestTTL returns the smaller of the non-zero values, or a default when
// both are zero.
func smallestTTL(a, b uint32) uint32 {
	switch {
	case a == 0 && b == 0:
		return uint32(cacheMax / time.Second)
	case a == 0:
		return b
	case b == 0:
		return a
	}
	return min(a, b)
}

// resolutionCache holds recent resolutions. It exists so that the AAAA and
// HTTPS queries that follow every A query reuse its decision instead of asking
// upstream again, and so a page that references one host many times does not
// cost a lookup each time.
type resolutionCache struct {
	mu  sync.Mutex
	m   map[string]*resolution
	now func() time.Time
}

func newResolutionCache(now func() time.Time) *resolutionCache {
	return &resolutionCache{m: map[string]*resolution{}, now: now}
}

func (c *resolutionCache) get(name string) *resolution {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, ok := c.m[name]
	if !ok {
		return nil
	}
	if !c.now().Before(res.expires) {
		delete(c.m, name)
		return nil
	}
	return res
}

func (c *resolutionCache) put(name string, res *resolution) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= cacheEntries {
		now := c.now()
		for k, v := range c.m {
			if !now.Before(v.expires) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= cacheEntries {
			// Everything is still fresh. Start over rather than grow.
			c.m = map[string]*resolution{}
		}
	}
	c.m[name] = res
}
