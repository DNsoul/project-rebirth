package splitdns

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestECSDataEncoding(t *testing.T) {
	cases := []struct {
		prefix string
		want   []byte
	}{
		// family 1, source 24, scope 0, three address bytes
		{"77.88.8.0/24", []byte{0, 1, 24, 0, 77, 88, 8}},
		// only the bytes the prefix covers are sent
		{"10.0.0.0/8", []byte{0, 1, 8, 0, 10}},
		// an unaligned prefix keeps a whole last byte
		{"77.88.8.0/21", []byte{0, 1, 21, 0, 77, 88, 8}},
		// host bits are masked off, so nothing beyond the prefix leaks
		{"77.88.8.77/24", []byte{0, 1, 24, 0, 77, 88, 8}},
	}
	for _, tc := range cases {
		got := ecsData(netip.MustParsePrefix(tc.prefix))
		if string(got) != string(tc.want) {
			t.Errorf("ecsData(%s) = %v, want %v", tc.prefix, got, tc.want)
		}
	}
}

func TestBuildQueryCarriesECS(t *testing.T) {
	u := &Upstream{Addr: "x:53", ECS: netip.MustParsePrefix("77.88.8.0/24")}
	raw, id, err := u.buildQuery("Example.COM", dnsmessage.TypeA)
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}

	var msg dnsmessage.Message
	if err := msg.Unpack(raw); err != nil {
		t.Fatalf("the query does not parse: %v", err)
	}
	if msg.ID != id || !msg.RecursionDesired {
		t.Errorf("header = %+v", msg.Header)
	}
	if len(msg.Questions) != 1 || msg.Questions[0].Type != dnsmessage.TypeA {
		t.Errorf("questions = %+v", msg.Questions)
	}

	var opt *dnsmessage.OPTResource
	for _, r := range msg.Additionals {
		if body, ok := r.Body.(*dnsmessage.OPTResource); ok {
			opt = body
		}
	}
	if opt == nil {
		t.Fatal("no EDNS record in the query")
	}
	if len(opt.Options) != 1 || opt.Options[0].Code != ecsOptionCode {
		t.Fatalf("options = %+v, want one client-subnet option", opt.Options)
	}
	if string(opt.Options[0].Data) != string([]byte{0, 1, 24, 0, 77, 88, 8}) {
		t.Errorf("client subnet data = %v", opt.Options[0].Data)
	}
}

func TestBuildQueryWithoutECSSendsNone(t *testing.T) {
	u := &Upstream{Addr: "x:53"}
	raw, _, err := u.buildQuery("example.com", dnsmessage.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(raw); err != nil {
		t.Fatal(err)
	}
	for _, r := range msg.Additionals {
		if body, ok := r.Body.(*dnsmessage.OPTResource); ok && len(body.Options) != 0 {
			t.Errorf("a query without ECS carries options: %+v", body.Options)
		}
	}
}

func TestBuildQueryRejectsUnusableNames(t *testing.T) {
	u := &Upstream{Addr: "x:53"}
	for _, name := range []string{
		"",
		"a..b",
		string(make([]byte, 300)),
	} {
		if _, _, err := u.buildQuery(name, dnsmessage.TypeA); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
}

// fakeDNS is a tiny DNS server for the client to talk to. It answers A
// queries with a fixed address and can be told to truncate over UDP so the
// TCP fallback is exercised.
type fakeDNS struct {
	t        *testing.T
	udp      *net.UDPConn
	tcp      net.Listener
	truncate bool

	mu      sync.Mutex
	queries []dnsmessage.Message
	wrongID bool
}

func startFakeDNS(t *testing.T, truncate bool) *fakeDNS {
	t.Helper()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	// TCP on the same port number, as a real server would.
	tcp, err := net.Listen("tcp", udp.LocalAddr().String())
	if err != nil {
		udp.Close()
		t.Fatalf("listen tcp: %v", err)
	}

	f := &fakeDNS{t: t, udp: udp, tcp: tcp, truncate: truncate}
	t.Cleanup(func() { udp.Close(); tcp.Close() })
	go f.serveUDP()
	go f.serveTCP()
	return f
}

func (f *fakeDNS) addr() string { return f.udp.LocalAddr().String() }

func (f *fakeDNS) reply(raw []byte, overUDP bool) []byte {
	var req dnsmessage.Message
	if err := req.Unpack(raw); err != nil {
		return nil
	}
	f.mu.Lock()
	f.queries = append(f.queries, req)
	wrongID := f.wrongID
	f.mu.Unlock()

	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: req.ID, Response: true, RecursionAvailable: true,
			Truncated: overUDP && f.truncate,
		},
		Questions: req.Questions,
	}
	if wrongID {
		resp.ID++
	}
	if !(overUDP && f.truncate) {
		resp.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name: req.Questions[0].Name, Type: dnsmessage.TypeA,
				Class: dnsmessage.ClassINET, TTL: 120,
			},
			Body: &dnsmessage.AResource{A: [4]byte{77, 88, 8, 8}},
		}}
	}
	out, err := resp.Pack()
	if err != nil {
		f.t.Errorf("pack reply: %v", err)
	}
	return out
}

func (f *fakeDNS) serveUDP() {
	buf := make([]byte, 4096)
	for {
		n, from, err := f.udp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if out := f.reply(buf[:n], true); out != nil {
			_, _ = f.udp.WriteToUDP(out, from)
		}
	}
}

func (f *fakeDNS) serveTCP() {
	for {
		c, err := f.tcp.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			var lenBuf [2]byte
			if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
				return
			}
			raw := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
			if _, err := io.ReadFull(c, raw); err != nil {
				return
			}
			out := f.reply(raw, false)
			framed := make([]byte, 2, 2+len(out))
			binary.BigEndian.PutUint16(framed, uint16(len(out)))
			_, _ = c.Write(append(framed, out...))
		}(c)
	}
}

func (f *fakeDNS) seen() []dnsmessage.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dnsmessage.Message(nil), f.queries...)
}

func TestUpstreamQueryOverUDP(t *testing.T) {
	srv := startFakeDNS(t, false)
	u := &Upstream{Addr: srv.addr(), ECS: netip.MustParsePrefix("77.88.8.0/24"), Timeout: 2 * time.Second}

	msg, err := u.Query(context.Background(), "example.com", dnsmessage.TypeA)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	addrs, ttl := addrsOf(msg)
	if len(addrs) != 1 || addrs[0].String() != "77.88.8.8" {
		t.Errorf("addresses = %v", addrs)
	}
	if ttl != 120 {
		t.Errorf("ttl = %d, want 120", ttl)
	}

	// The server must actually have received the client subnet.
	seen := srv.seen()
	if len(seen) != 1 {
		t.Fatalf("server saw %d queries, want 1", len(seen))
	}
	var gotECS bool
	for _, r := range seen[0].Additionals {
		if body, ok := r.Body.(*dnsmessage.OPTResource); ok {
			for _, o := range body.Options {
				if o.Code == ecsOptionCode {
					gotECS = true
				}
			}
		}
	}
	if !gotECS {
		t.Error("the query reached the server without a client subnet")
	}
}

func TestUpstreamFallsBackToTCPWhenTruncated(t *testing.T) {
	srv := startFakeDNS(t, true)
	u := &Upstream{Addr: srv.addr(), Timeout: 2 * time.Second}

	msg, err := u.Query(context.Background(), "example.com", dnsmessage.TypeA)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	addrs, _ := addrsOf(msg)
	if len(addrs) != 1 {
		t.Fatalf("addresses = %v, want the full answer from the TCP retry", addrs)
	}
	if got := len(srv.seen()); got != 2 {
		t.Errorf("server saw %d queries, want 2 (UDP then TCP)", got)
	}
}

func TestUpstreamRejectsAWrongID(t *testing.T) {
	srv := startFakeDNS(t, false)
	srv.mu.Lock()
	srv.wrongID = true
	srv.mu.Unlock()

	u := &Upstream{Addr: srv.addr(), Timeout: 2 * time.Second}
	if _, err := u.Query(context.Background(), "example.com", dnsmessage.TypeA); err == nil {
		t.Fatal("a reply with the wrong ID was accepted")
	}
}

func TestUpstreamTimesOut(t *testing.T) {
	// A socket that reads and never answers.
	silent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { silent.Close() })

	u := &Upstream{Addr: silent.LocalAddr().String(), Timeout: 150 * time.Millisecond}
	start := time.Now()
	if _, err := u.Query(context.Background(), "example.com", dnsmessage.TypeA); err == nil {
		t.Fatal("a silent server did not produce an error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("gave up after %s, the timeout was 150ms", elapsed)
	}
}

func TestCheckReplyRefusesOtherPeoplesAnswers(t *testing.T) {
	pack := func(m dnsmessage.Message) []byte {
		raw, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	q := func(name string, typ dnsmessage.Type) []dnsmessage.Question {
		return []dnsmessage.Question{{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}}
	}

	good := pack(dnsmessage.Message{Header: dnsmessage.Header{ID: 7, Response: true}, Questions: q("example.com.", dnsmessage.TypeA)})
	if _, err := checkReply(good, 7, "example.com", dnsmessage.TypeA); err != nil {
		t.Errorf("a valid reply was refused: %v", err)
	}
	// Case in the echoed question is not significant.
	mixed := pack(dnsmessage.Message{Header: dnsmessage.Header{ID: 7, Response: true}, Questions: q("EXAMPLE.com.", dnsmessage.TypeA)})
	if _, err := checkReply(mixed, 7, "example.com", dnsmessage.TypeA); err != nil {
		t.Errorf("a reply with different case was refused: %v", err)
	}

	for name, raw := range map[string][]byte{
		"wrong id":       pack(dnsmessage.Message{Header: dnsmessage.Header{ID: 8, Response: true}, Questions: q("example.com.", dnsmessage.TypeA)}),
		"not a response": pack(dnsmessage.Message{Header: dnsmessage.Header{ID: 7}, Questions: q("example.com.", dnsmessage.TypeA)}),
		"other name":     pack(dnsmessage.Message{Header: dnsmessage.Header{ID: 7, Response: true}, Questions: q("evil.com.", dnsmessage.TypeA)}),
		"other type":     pack(dnsmessage.Message{Header: dnsmessage.Header{ID: 7, Response: true}, Questions: q("example.com.", dnsmessage.TypeAAAA)}),
		"no question":    pack(dnsmessage.Message{Header: dnsmessage.Header{ID: 7, Response: true}}),
		"garbage":        []byte("not dns at all"),
	} {
		if _, err := checkReply(raw, 7, "example.com", dnsmessage.TypeA); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAddrsOfIgnoresCNAMEsAndTakesTheSmallestTTL(t *testing.T) {
	name := dnsmessage.MustNewName("www.example.com.")
	msg := &dnsmessage.Message{Answers: []dnsmessage.Resource{
		{Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 5},
			Body: &dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("cdn.example.net.")}},
		{Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 300},
			Body: &dnsmessage.AResource{A: [4]byte{1, 2, 3, 4}}},
		{Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
			Body: &dnsmessage.AResource{A: [4]byte{5, 6, 7, 8}}},
	}}
	addrs, ttl := addrsOf(msg)
	if len(addrs) != 2 || addrs[0].String() != "1.2.3.4" || addrs[1].String() != "5.6.7.8" {
		t.Errorf("addresses = %v", addrs)
	}
	if ttl != 60 {
		t.Errorf("ttl = %d, want the smallest A record TTL (60), not the CNAME's", ttl)
	}
	if a, ttl := addrsOf(nil); a != nil || ttl != 0 {
		t.Errorf("addrsOf(nil) = %v, %d", a, ttl)
	}
	if a, ttl := addrsOf(&dnsmessage.Message{}); len(a) != 0 || ttl != 0 {
		t.Errorf("an empty answer gave %v, %d", a, ttl)
	}
}
