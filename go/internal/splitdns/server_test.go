package splitdns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeResolver answers from a table instead of the network.
type fakeResolver struct {
	mu      sync.Mutex
	a       map[string][]string // name -> A addresses
	aaaa    map[string]bool     // names that have an AAAA record
	nx      map[string]bool     // names that do not exist
	ttl     uint32
	failAll error
	calls   map[string]int // "name/type" -> count
}

func newResolver() *fakeResolver {
	return &fakeResolver{
		a: map[string][]string{}, aaaa: map[string]bool{}, nx: map[string]bool{},
		ttl: 120, calls: map[string]int{},
	}
}

func (f *fakeResolver) Query(_ context.Context, name string, qtype dnsmessage.Type) (*dnsmessage.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[fmt.Sprintf("%s/%d", name, qtype)]++
	if f.failAll != nil {
		return nil, f.failAll
	}

	qn := dnsmessage.MustNewName(name + ".")
	msg := &dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true},
		Questions: []dnsmessage.Question{{Name: qn, Type: qtype, Class: dnsmessage.ClassINET}},
	}
	if f.nx[name] {
		msg.RCode = dnsmessage.RCodeNameError
		return msg, nil
	}
	hdr := dnsmessage.ResourceHeader{Name: qn, Class: dnsmessage.ClassINET, TTL: f.ttl}
	switch qtype {
	case dnsmessage.TypeA:
		for _, s := range f.a[name] {
			h := hdr
			h.Type = dnsmessage.TypeA
			msg.Answers = append(msg.Answers, dnsmessage.Resource{
				Header: h, Body: &dnsmessage.AResource{A: netip.MustParseAddr(s).As4()},
			})
		}
	case dnsmessage.TypeAAAA:
		if f.aaaa[name] {
			h := hdr
			h.Type = dnsmessage.TypeAAAA
			msg.Answers = append(msg.Answers, dnsmessage.Resource{
				Header: h, Body: &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("2001:db8::1").As16()},
			})
		}
	case dnsmessage.TypeTXT:
		h := hdr
		h.Type = dnsmessage.TypeTXT
		msg.Answers = append(msg.Answers, dnsmessage.Resource{
			Header: h, Body: &dnsmessage.TXTResource{TXT: []string{"hello"}},
		})
	}
	return msg, nil
}

func (f *fakeResolver) count(name string, qtype dnsmessage.Type) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[fmt.Sprintf("%s/%d", name, qtype)]
}

// fakeMapper records what would have been installed in the kernel.
type fakeMapper struct {
	mu      sync.Mutex
	ops     []string
	table   map[netip.Addr]netip.Addr
	failAdd error
	failDel error
}

func newMapper() *fakeMapper { return &fakeMapper{table: map[netip.Addr]netip.Addr{}} }

func (m *fakeMapper) Add(fake, real netip.Addr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAdd != nil {
		return m.failAdd
	}
	m.ops = append(m.ops, "add "+fake.String())
	m.table[fake] = real
	return nil
}

func (m *fakeMapper) Delete(fake netip.Addr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failDel != nil {
		return m.failDel
	}
	m.ops = append(m.ops, "del "+fake.String())
	delete(m.table, fake)
	return nil
}

func (m *fakeMapper) adds() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, op := range m.ops {
		if strings.HasPrefix(op, "add ") {
			n++
		}
	}
	return n
}

type rig struct {
	srv       *Server
	primary   *fakeResolver
	secondary *fakeResolver
	mapper    *fakeMapper
	pool      *Pool
	clock     *time.Time
}

type rigOption func(*rigConfig)

type rigConfig struct {
	pool     *Pool
	forceVPN []string
	forceDir []string
	log      *slog.Logger
	noSecond bool
}

func newRig(t *testing.T, opts ...rigOption) *rig {
	t.Helper()
	var rc rigConfig
	for _, o := range opts {
		o(&rc)
	}

	pool := rc.pool
	if pool == nil {
		var err error
		pool, err = NewPool(netip.MustParsePrefix("198.18.0.0/15"), netip.MustParseAddr("198.18.0.1"))
		if err != nil {
			t.Fatal(err)
		}
	}

	r := &rig{
		primary: newResolver(), secondary: newResolver(),
		mapper: newMapper(), pool: pool,
	}
	now := time.Unix(1_800_000_000, 0)
	r.clock = &now

	cfg := Config{
		Decider: &Decider{
			RU:          localSet(t),
			ForceTunnel: mustRules(t, rc.forceVPN...),
			ForceDirect: mustRules(t, rc.forceDir...),
		},
		Pool:    pool,
		Mapper:  r.mapper,
		Primary: r.primary,
		Log:     rc.log,
		Now:     func() time.Time { return *r.clock },
	}
	if !rc.noSecond {
		cfg.Secondary = r.secondary
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.srv = srv
	return r
}

// both gives the same A answer to both views, which is the ordinary case.
func (r *rig) both(name string, addrs ...string) {
	r.primary.a[name] = addrs
	r.secondary.a[name] = addrs
}

func (r *rig) ask(t *testing.T, name string, qtype dnsmessage.Type) *dnsmessage.Message {
	t.Helper()
	return r.askRaw(t, packQuery(t, name, qtype))
}

func (r *rig) askRaw(t *testing.T, raw []byte) *dnsmessage.Message {
	t.Helper()
	out := r.srv.Handle(context.Background(), raw)
	if out == nil {
		return nil
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(out); err != nil {
		t.Fatalf("the server produced an unreadable reply: %v", err)
	}
	return &msg
}

func packQuery(t *testing.T, name string, qtype dnsmessage.Type) []byte {
	t.Helper()
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x4242, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(name + "."), Type: qtype, Class: dnsmessage.ClassINET}},
	}
	raw, err := msg.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func answerAddrs(msg *dnsmessage.Message) []string {
	var out []string
	for _, r := range msg.Answers {
		if a, ok := r.Body.(*dnsmessage.AResource); ok {
			out = append(out, netip.AddrFrom4(a.A).String())
		}
	}
	return out
}

func inStandInRange(s string) bool {
	return netip.MustParsePrefix("198.18.0.0/15").Contains(netip.MustParseAddr(s))
}

func TestDirectSiteGetsRealAddresses(t *testing.T) {
	r := newRig(t)
	r.both("site.ru", "5.1.1.1", "77.88.1.1")

	msg := r.ask(t, "site.ru", dnsmessage.TypeA)
	if msg.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("rcode = %v", msg.RCode)
	}
	got := answerAddrs(msg)
	if len(got) != 2 || got[0] != "5.1.1.1" || got[1] != "77.88.1.1" {
		t.Errorf("a direct site was given %v, want its real addresses", got)
	}
	if r.mapper.adds() != 0 {
		t.Error("a direct site caused a translation to be installed")
	}
	if st := r.srv.Stats(); st.Direct != 1 || st.Tunnel != 0 {
		t.Errorf("stats = %+v", st)
	}
	// The reply must carry the ID and question of the query.
	if msg.ID != 0x4242 || !msg.Response || !msg.RecursionAvailable {
		t.Errorf("header = %+v", msg.Header)
	}
}

func TestForeignSiteGetsStandIns(t *testing.T) {
	r := newRig(t)
	r.both("site.com", "142.250.1.1")

	msg := r.ask(t, "site.com", dnsmessage.TypeA)
	got := answerAddrs(msg)
	if len(got) != 1 || !inStandInRange(got[0]) {
		t.Fatalf("a tunnelled site was given %v, want an address in 198.18.0.0/15", got)
	}
	// The stand-in must translate to the real address, or traffic goes nowhere.
	real := r.mapper.table[netip.MustParseAddr(got[0])]
	if real.String() != "142.250.1.1" {
		t.Errorf("%s translates to %s, want 142.250.1.1", got[0], real)
	}
	if st := r.srv.Stats(); st.Tunnel != 1 || st.PoolSize != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestStandInIsStableAndInstalledOnce(t *testing.T) {
	r := newRig(t)
	r.both("a.com", "142.250.1.1")
	r.both("b.com", "142.250.1.1") // a different name on the same server

	first := answerAddrs(r.ask(t, "a.com", dnsmessage.TypeA))
	*r.clock = r.clock.Add(time.Hour) // let the cache lapse, force a fresh lookup
	again := answerAddrs(r.ask(t, "a.com", dnsmessage.TypeA))
	other := answerAddrs(r.ask(t, "b.com", dnsmessage.TypeA))

	if first[0] != again[0] {
		t.Errorf("the same site got %s then %s", first[0], again[0])
	}
	if first[0] != other[0] {
		t.Errorf("two names on one server got %s and %s, want a shared stand-in", first[0], other[0])
	}
	if r.mapper.adds() != 1 {
		t.Errorf("the translation was installed %d times, want once", r.mapper.adds())
	}
}

// One foreign record is enough: the whole name goes through the tunnel.
func TestMixedSiteGoesThroughTheTunnel(t *testing.T) {
	r := newRig(t)
	r.both("mixed.ru", "5.1.1.1", "142.250.1.1")

	got := answerAddrs(r.ask(t, "mixed.ru", dnsmessage.TypeA))
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	for _, a := range got {
		if !inStandInRange(a) {
			t.Errorf("%s is not a stand-in: a mixed site leaked a real address", a)
		}
	}
}

// The second view exists to catch a site that shows a local client one thing
// and everyone else another.
func TestForeignAddressInEitherViewForcesTheTunnel(t *testing.T) {
	r := newRig(t)
	r.primary.a["cdn.ru"] = []string{"5.1.1.1"}       // what a local client sees
	r.secondary.a["cdn.ru"] = []string{"142.250.1.1"} // what everyone else sees

	got := answerAddrs(r.ask(t, "cdn.ru", dnsmessage.TypeA))
	if len(got) == 0 || !inStandInRange(got[0]) {
		t.Errorf("a site with a foreign address in one view went direct: %v", got)
	}
}

func TestDirectSiteIsGivenTheLocalView(t *testing.T) {
	r := newRig(t)
	r.primary.a["site.ru"] = []string{"5.1.1.1"}
	r.secondary.a["site.ru"] = []string{"77.88.9.9"}

	got := answerAddrs(r.ask(t, "site.ru", dnsmessage.TypeA))
	if len(got) != 1 || got[0] != "5.1.1.1" {
		t.Errorf("got %v, want the primary (local) view [5.1.1.1]", got)
	}
}

func TestForceRules(t *testing.T) {
	r := newRig(t, func(c *rigConfig) {
		c.forceVPN = []string{"tracker.ru"}
		c.forceDir = []string{"bank.com"}
	})
	r.both("tracker.ru", "5.1.1.1")   // local address, forced into the tunnel
	r.both("bank.com", "142.250.1.1") // foreign address, forced direct

	if got := answerAddrs(r.ask(t, "tracker.ru", dnsmessage.TypeA)); !inStandInRange(got[0]) {
		t.Errorf("a forced-tunnel site was given %v", got)
	}
	if got := answerAddrs(r.ask(t, "bank.com", dnsmessage.TypeA)); got[0] != "142.250.1.1" {
		t.Errorf("a forced-direct site was given %v", got)
	}
}

func TestNonexistentNamePassesThrough(t *testing.T) {
	r := newRig(t)
	r.primary.nx["nope.example"] = true
	r.secondary.nx["nope.example"] = true

	msg := r.ask(t, "nope.example", dnsmessage.TypeA)
	if msg.RCode != dnsmessage.RCodeNameError {
		t.Errorf("rcode = %v, want NXDOMAIN", msg.RCode)
	}
	if len(msg.Answers) != 0 {
		t.Errorf("an NXDOMAIN carried answers: %v", msg.Answers)
	}
	if r.mapper.adds() != 0 {
		t.Error("a nonexistent name caused a translation")
	}
}

func TestNameWithNoAddressIsNODATA(t *testing.T) {
	r := newRig(t)
	r.both("empty.example") // exists, no A records

	msg := r.ask(t, "empty.example", dnsmessage.TypeA)
	if msg.RCode != dnsmessage.RCodeSuccess || len(msg.Answers) != 0 {
		t.Errorf("got rcode %v with %d answers, want an empty success", msg.RCode, len(msg.Answers))
	}
}

func TestOneFailedUpstreamStillAnswers(t *testing.T) {
	r := newRig(t)
	r.both("site.com", "142.250.1.1")
	r.secondary.failAll = errors.New("timeout")

	msg := r.ask(t, "site.com", dnsmessage.TypeA)
	if msg.RCode != dnsmessage.RCodeSuccess || len(answerAddrs(msg)) != 1 {
		t.Errorf("with one upstream down got rcode %v, answers %v", msg.RCode, answerAddrs(msg))
	}

	r2 := newRig(t)
	r2.both("site.com", "142.250.1.1")
	r2.primary.failAll = errors.New("timeout")
	if msg := r2.ask(t, "site.com", dnsmessage.TypeA); msg.RCode != dnsmessage.RCodeSuccess {
		t.Errorf("with the primary down got rcode %v", msg.RCode)
	}
}

func TestEveryUpstreamFailingIsSERVFAIL(t *testing.T) {
	r := newRig(t)
	r.primary.failAll = errors.New("timeout")
	r.secondary.failAll = errors.New("timeout")

	msg := r.ask(t, "site.com", dnsmessage.TypeA)
	if msg.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("rcode = %v, want SERVFAIL", msg.RCode)
	}
	if st := r.srv.Stats(); st.Failures != 1 {
		t.Errorf("Failures = %d, want 1", st.Failures)
	}
}

func TestWorksWithoutASecondaryResolver(t *testing.T) {
	r := newRig(t, func(c *rigConfig) { c.noSecond = true })
	r.primary.a["site.com"] = []string{"142.250.1.1"}
	if got := answerAddrs(r.ask(t, "site.com", dnsmessage.TypeA)); len(got) != 1 || !inStandInRange(got[0]) {
		t.Errorf("got %v", got)
	}
}

// AAAA and the HTTPS hint must not hand a real address to an application that
// was meant to use the stand-in.
func TestAddressLikeRecordsAreSuppressedForTunnelledNames(t *testing.T) {
	r := newRig(t)
	r.both("site.com", "142.250.1.1")
	r.primary.aaaa["site.com"] = true

	for _, qt := range []dnsmessage.Type{dnsmessage.TypeAAAA, typeHTTPS, typeSVCB} {
		msg := r.ask(t, "site.com", qt)
		if msg.RCode != dnsmessage.RCodeSuccess || len(msg.Answers) != 0 {
			t.Errorf("type %d for a tunnelled name: rcode %v, %d answers; want an empty success",
				qt, msg.RCode, len(msg.Answers))
		}
	}
}

func TestAAAAIsPassedThroughForDirectNames(t *testing.T) {
	r := newRig(t)
	r.both("site.ru", "5.1.1.1")
	r.primary.aaaa["site.ru"] = true

	msg := r.ask(t, "site.ru", dnsmessage.TypeAAAA)
	if len(msg.Answers) != 1 {
		t.Fatalf("a direct name lost its AAAA record: %d answers", len(msg.Answers))
	}
	if _, ok := msg.Answers[0].Body.(*dnsmessage.AAAAResource); !ok {
		t.Errorf("the answer is a %T", msg.Answers[0].Body)
	}
}

func TestAAAAForNonexistentNameIsNXDOMAIN(t *testing.T) {
	r := newRig(t)
	r.primary.nx["nope.example"] = true
	r.secondary.nx["nope.example"] = true
	if msg := r.ask(t, "nope.example", dnsmessage.TypeAAAA); msg.RCode != dnsmessage.RCodeNameError {
		t.Errorf("rcode = %v", msg.RCode)
	}
}

func TestOtherTypesAreForwarded(t *testing.T) {
	r := newRig(t)
	r.both("site.com", "142.250.1.1")

	msg := r.ask(t, "site.com", dnsmessage.TypeTXT)
	if len(msg.Answers) != 1 {
		t.Fatalf("a TXT query got %d answers", len(msg.Answers))
	}
	if txt, ok := msg.Answers[0].Body.(*dnsmessage.TXTResource); !ok || txt.TXT[0] != "hello" {
		t.Errorf("answer = %+v", msg.Answers[0].Body)
	}
}

// The security property of the whole scheme. A stand-in translates to
// whatever address the answer contained, so an answer naming the server's own
// services must never be turned into one.
func TestServiceAddressesAreNeverTranslated(t *testing.T) {
	for _, bad := range []string{
		"169.254.169.254", // cloud metadata
		"127.0.0.1",
		"10.8.0.1", // the server's own tunnel address
		"192.168.1.1",
		"198.18.0.1", // the resolver itself
		"0.0.0.0",
	} {
		r := newRig(t)
		r.both("evil.example", bad)

		msg := r.ask(t, "evil.example", dnsmessage.TypeA)
		if len(msg.Answers) != 0 {
			t.Errorf("%s: an answer carrying it was given: %v", bad, answerAddrs(msg))
		}
		if r.mapper.adds() != 0 {
			t.Errorf("%s: a translation was installed for it", bad)
		}
		if st := r.srv.Stats(); st.Filtered == 0 {
			t.Errorf("%s: the filtered counter did not move", bad)
		}
	}
}

func TestOnlyTheServiceAddressIsDroppedFromAMixedAnswer(t *testing.T) {
	r := newRig(t)
	r.both("evil.example", "169.254.169.254", "142.250.1.1")

	got := answerAddrs(r.ask(t, "evil.example", dnsmessage.TypeA))
	if len(got) != 1 {
		t.Fatalf("got %v, want the one public address as a stand-in", got)
	}
	if real := r.mapper.table[netip.MustParseAddr(got[0])]; real.String() != "142.250.1.1" {
		t.Errorf("the stand-in translates to %s", real)
	}
}

func TestFailedInstallIsNotHandedOutOrRemembered(t *testing.T) {
	r := newRig(t)
	r.both("site.com", "142.250.1.1")
	r.mapper.failAdd = errors.New("nft exploded")

	msg := r.ask(t, "site.com", dnsmessage.TypeA)
	if msg.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("rcode = %v, want SERVFAIL", msg.RCode)
	}
	if len(msg.Answers) != 0 {
		t.Errorf("a stand-in with no translation behind it was handed out: %v", answerAddrs(msg))
	}
	if r.pool.Len() != 0 {
		t.Errorf("the pool kept %d entries for a translation that was never installed", r.pool.Len())
	}

	// Once the kernel recovers, the same site has to work.
	r.mapper.failAdd = nil
	if got := answerAddrs(r.ask(t, "site.com", dnsmessage.TypeA)); len(got) != 1 {
		t.Errorf("did not recover after the install failure: %v", got)
	}
}

func TestEvictionRemovesTheOldTranslationBeforeAddingTheNew(t *testing.T) {
	pool, err := NewPool(netip.MustParsePrefix("198.18.0.0/30"), netip.MustParseAddr("198.18.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	// A /30 has two usable addresses and one is reserved, leaving one.
	r := newRig(t, func(c *rigConfig) { c.pool = pool })
	r.both("a.com", "8.8.8.1")
	r.both("b.com", "8.8.8.2")

	first := answerAddrs(r.ask(t, "a.com", dnsmessage.TypeA))
	*r.clock = r.clock.Add(time.Minute)
	second := answerAddrs(r.ask(t, "b.com", dnsmessage.TypeA))

	if first[0] != second[0] {
		t.Fatalf("expected the only slot to be recycled, got %s then %s", first[0], second[0])
	}
	// The order matters: remove, then add.
	want := []string{"add " + first[0], "del " + first[0], "add " + second[0]}
	if strings.Join(r.mapper.ops, ",") != strings.Join(want, ",") {
		t.Errorf("operations = %v, want %v", r.mapper.ops, want)
	}
	if real := r.mapper.table[netip.MustParseAddr(second[0])]; real.String() != "8.8.8.2" {
		t.Errorf("the recycled stand-in translates to %s", real)
	}
}

func TestFailedRemovalOfARecycledTranslationIsAnError(t *testing.T) {
	pool, err := NewPool(netip.MustParsePrefix("198.18.0.0/30"), netip.MustParseAddr("198.18.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, func(c *rigConfig) { c.pool = pool })
	r.both("a.com", "8.8.8.1")
	r.both("b.com", "8.8.8.2")

	r.ask(t, "a.com", dnsmessage.TypeA)
	*r.clock = r.clock.Add(time.Minute)
	r.mapper.failDel = errors.New("nft exploded")

	// If the old translation cannot be removed, pointing the address at a new
	// target would briefly deliver traffic to the wrong place.
	msg := r.ask(t, "b.com", dnsmessage.TypeA)
	if msg.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("rcode = %v, want SERVFAIL", msg.RCode)
	}
}

func TestCanaryNamesAreNXDOMAIN(t *testing.T) {
	r := newRig(t)
	r.both("use-application-dns.net", "142.250.1.1")

	for _, name := range []string{"use-application-dns.net", "mask.icloud.com", "MASK-H2.icloud.com"} {
		for _, qt := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
			if msg := r.ask(t, name, qt); msg.RCode != dnsmessage.RCodeNameError {
				t.Errorf("%s type %d: rcode = %v, want NXDOMAIN", name, qt, msg.RCode)
			}
		}
	}
}

func TestBadPacketsAreHandledSafely(t *testing.T) {
	r := newRig(t)

	if r.srv.Handle(context.Background(), []byte("garbage")) != nil {
		t.Error("garbage was answered")
	}
	if r.srv.Handle(context.Background(), nil) != nil {
		t.Error("an empty packet was answered")
	}

	// A response must never be answered: two resolvers would loop forever.
	resp := packQuery(t, "site.com", dnsmessage.TypeA)
	resp[2] |= 0x80
	if r.srv.Handle(context.Background(), resp) != nil {
		t.Error("a response packet was answered")
	}

	twoQ := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 1},
		Questions: []dnsmessage.Question{
			{Name: dnsmessage.MustNewName("a.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
			{Name: dnsmessage.MustNewName("b.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
		},
	}
	raw, _ := twoQ.Pack()
	if msg := r.askRaw(t, raw); msg == nil || msg.RCode != dnsmessage.RCodeFormatError {
		t.Errorf("two questions: %+v", msg)
	}

	chaos := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 2},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("version.bind."), Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassCHAOS}},
	}
	raw, _ = chaos.Pack()
	if msg := r.askRaw(t, raw); msg == nil || msg.RCode != dnsmessage.RCodeRefused {
		t.Errorf("CHAOS class: %+v", msg)
	}

	status := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 3, OpCode: 2},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("a.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	raw, _ = status.Pack()
	if msg := r.askRaw(t, raw); msg == nil || msg.RCode != dnsmessage.RCodeNotImplemented {
		t.Errorf("unsupported opcode: %+v", msg)
	}
}

func TestCacheAvoidsRepeatedLookups(t *testing.T) {
	r := newRig(t)
	r.both("site.com", "142.250.1.1")

	r.ask(t, "site.com", dnsmessage.TypeA)
	r.ask(t, "site.com", dnsmessage.TypeA)
	r.ask(t, "site.com", dnsmessage.TypeAAAA) // reuses the decision made for the A query
	r.ask(t, "site.com", typeHTTPS)

	if got := r.primary.count("site.com", dnsmessage.TypeA); got != 1 {
		t.Errorf("upstream was asked %d times, want 1", got)
	}

	// After the entry lapses it has to be asked again.
	*r.clock = r.clock.Add(2 * time.Minute)
	r.ask(t, "site.com", dnsmessage.TypeA)
	if got := r.primary.count("site.com", dnsmessage.TypeA); got != 2 {
		t.Errorf("after expiry upstream was asked %d times, want 2", got)
	}
}

func TestTTLsAreCapped(t *testing.T) {
	r := newRig(t)
	r.primary.ttl, r.secondary.ttl = 86400, 86400
	r.both("site.ru", "5.1.1.1")
	r.both("site.com", "142.250.1.1")

	direct := r.ask(t, "site.ru", dnsmessage.TypeA)
	if ttl := direct.Answers[0].Header.TTL; ttl != defaultMaxDirectTTL {
		t.Errorf("direct TTL = %d, want it capped at %d", ttl, defaultMaxDirectTTL)
	}
	tunnel := r.ask(t, "site.com", dnsmessage.TypeA)
	if ttl := tunnel.Answers[0].Header.TTL; ttl != defaultTunnelTTL {
		t.Errorf("tunnel TTL = %d, want %d", ttl, defaultTunnelTTL)
	}
}

func TestStandInsAreLimited(t *testing.T) {
	r := newRig(t)
	r.both("big.com", "8.8.8.1", "8.8.8.2", "8.8.8.3", "8.8.8.4", "8.8.8.5", "8.8.8.6")

	got := answerAddrs(r.ask(t, "big.com", dnsmessage.TypeA))
	if len(got) != defaultMaxStandIns {
		t.Errorf("got %d stand-ins, want the limit of %d", len(got), defaultMaxStandIns)
	}
}

// The resolver sees every site the user visits. At the default log level none
// of that may reach the log, including when a lookup fails.
func TestLogsNeverContainVisitedNames(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	r := newRig(t, func(c *rigConfig) { c.log = log })
	r.both("secret-site.example", "142.250.1.1")
	r.both("failing-site.example", "142.250.1.2")

	r.ask(t, "secret-site.example", dnsmessage.TypeA)
	r.primary.failAll = errors.New("timeout")
	r.secondary.failAll = errors.New("timeout")
	r.ask(t, "failing-site.example", dnsmessage.TypeA) // fails and logs a warning

	logged := buf.String()
	if logged == "" {
		t.Fatal("nothing was logged, so this test proves nothing")
	}
	for _, name := range []string{"secret-site", "failing-site"} {
		if strings.Contains(logged, name) {
			t.Errorf("the log contains %q:\n%s", name, logged)
		}
	}
}

func TestNewServerValidatesItsConfig(t *testing.T) {
	pool, _ := NewPool(netip.MustParsePrefix("198.18.0.0/15"))
	good := Config{Decider: &Decider{}, Pool: pool, Mapper: newMapper(), Primary: newResolver()}

	if _, err := NewServer(good); err != nil {
		t.Fatalf("a complete config was refused: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no decider":  func(c *Config) { c.Decider = nil },
		"no pool":     func(c *Config) { c.Pool = nil },
		"no mapper":   func(c *Config) { c.Mapper = nil },
		"no resolver": func(c *Config) { c.Primary = nil },
	} {
		cfg := good
		mutate(&cfg)
		if _, err := NewServer(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSmallestTTL(t *testing.T) {
	for _, tc := range []struct{ a, b, want uint32 }{
		{0, 0, 60}, {30, 0, 30}, {0, 30, 30}, {30, 90, 30}, {90, 30, 30},
	} {
		if got := smallestTTL(tc.a, tc.b); got != tc.want {
			t.Errorf("smallestTTL(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
