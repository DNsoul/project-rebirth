package splitdns

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// bigResolver answers every TXT query with an answer far larger than the 512
// bytes a plain UDP reply may carry.
type bigResolver struct{ fakeResolver }

func (b *bigResolver) Query(ctx context.Context, name string, qtype dnsmessage.Type) (*dnsmessage.Message, error) {
	if qtype != dnsmessage.TypeTXT {
		return b.fakeResolver.Query(ctx, name, qtype)
	}
	strs := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		strs = append(strs, strings.Repeat("x", 200))
	}
	qn := dnsmessage.MustNewName(name + ".")
	return &dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true},
		Questions: []dnsmessage.Question{{Name: qn, Type: qtype, Class: dnsmessage.ClassINET}},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: qn, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &dnsmessage.TXTResource{TXT: strs},
		}},
	}, nil
}

// live is a running server on loopback sockets.
type live struct {
	rig    *rig
	udp    *net.UDPConn
	tcp    net.Listener
	cancel context.CancelFunc
	done   chan error
}

func startLive(t *testing.T, srv *Server, rg *rig) *live {
	t.Helper()
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		udp.Close()
		t.Fatalf("listen tcp: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	l := &live{rig: rg, udp: udp, tcp: tcp, cancel: cancel, done: make(chan error, 1)}
	go func() { l.done <- srv.Serve(ctx, udp, tcp) }()
	t.Cleanup(func() { cancel(); <-l.done })
	return l
}

func (l *live) udpAddr() string { return l.udp.LocalAddr().String() }
func (l *live) tcpAddr() string { return l.tcp.Addr().String() }

func exchangeUDP(t *testing.T, addr string, query []byte) *dnsmessage.Message {
	t.Helper()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(query); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no UDP reply: %v", err)
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(buf[:n]); err != nil {
		t.Fatalf("unreadable UDP reply: %v", err)
	}
	return &msg
}

func exchangeTCP(t *testing.T, conn net.Conn, query []byte) *dnsmessage.Message {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	framed := make([]byte, 2, 2+len(query))
	binary.BigEndian.PutUint16(framed, uint16(len(query)))
	if _, err := conn.Write(append(framed, query...)); err != nil {
		t.Fatal(err)
	}
	var lenBuf [2]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		t.Fatalf("no TCP reply: %v", err)
	}
	raw := make([]byte, binary.BigEndian.Uint16(lenBuf[:]))
	if _, err := io.ReadFull(conn, raw); err != nil {
		t.Fatal(err)
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(raw); err != nil {
		t.Fatalf("unreadable TCP reply: %v", err)
	}
	return &msg
}

// A real DNS client, the one the operating system uses, must be able to talk
// to this server and see the two kinds of answer.
func TestEndToEndWithTheGoResolver(t *testing.T) {
	r := newRig(t)
	r.both("site.ru", "5.1.1.1")
	r.both("site.com", "142.250.1.1")
	l := startLive(t, r.srv, r)

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", l.udpAddr())
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	direct, err := resolver.LookupHost(ctx, "site.ru")
	if err != nil {
		t.Fatalf("LookupHost(site.ru): %v", err)
	}
	if len(direct) != 1 || direct[0] != "5.1.1.1" {
		t.Errorf("site.ru resolved to %v, want its real address", direct)
	}

	tunnelled, err := resolver.LookupHost(ctx, "site.com")
	if err != nil {
		t.Fatalf("LookupHost(site.com): %v", err)
	}
	if len(tunnelled) != 1 || !inStandInRange(tunnelled[0]) {
		t.Errorf("site.com resolved to %v, want a stand-in", tunnelled)
	}
}

func TestEndToEndOverTCPReusesTheConnection(t *testing.T) {
	r := newRig(t)
	r.both("site.com", "142.250.1.1")
	r.both("site.ru", "5.1.1.1")
	l := startLive(t, r.srv, r)

	conn, err := net.Dial("tcp", l.tcpAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Two queries on one connection.
	first := exchangeTCP(t, conn, packQuery(t, "site.com", dnsmessage.TypeA))
	second := exchangeTCP(t, conn, packQuery(t, "site.ru", dnsmessage.TypeA))

	if got := answerAddrs(first); len(got) != 1 || !inStandInRange(got[0]) {
		t.Errorf("first answer = %v", got)
	}
	if got := answerAddrs(second); len(got) != 1 || got[0] != "5.1.1.1" {
		t.Errorf("second answer = %v", got)
	}
}

func TestLargeAnswerIsTruncatedOverUDPAndCompleteOverTCP(t *testing.T) {
	r := newRig(t)
	big := &bigResolver{*newResolver()}
	srv, err := NewServer(Config{
		Decider: &Decider{RU: localSet(t)},
		Pool:    r.pool,
		Mapper:  r.mapper,
		Primary: big,
	})
	if err != nil {
		t.Fatal(err)
	}
	l := startLive(t, srv, r)

	query := packQuery(t, "big.example", dnsmessage.TypeTXT)

	udp := exchangeUDP(t, l.udpAddr(), query)
	if !udp.Truncated {
		t.Error("an answer over 512 bytes was not marked truncated over UDP")
	}
	if len(udp.Answers) != 0 {
		t.Errorf("a truncated reply carried %d answers", len(udp.Answers))
	}

	conn, err := net.Dial("tcp", l.tcpAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tcp := exchangeTCP(t, conn, query)
	if tcp.Truncated || len(tcp.Answers) != 1 {
		t.Errorf("TCP reply: truncated=%v answers=%d, want the full answer", tcp.Truncated, len(tcp.Answers))
	}
}

func TestUDPLimit(t *testing.T) {
	plain := packQuery(t, "a.com", dnsmessage.TypeA)
	if got := udpLimit(plain); got != 512 {
		t.Errorf("a query without EDNS has limit %d, want 512", got)
	}

	withEDNS := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 1},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("a.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	opt := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(".")}, Body: &dnsmessage.OPTResource{}}
	if err := opt.Header.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
		t.Fatal(err)
	}
	withEDNS.Additionals = []dnsmessage.Resource{opt}
	raw, err := withEDNS.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if got := udpLimit(raw); got != 1232 {
		t.Errorf("a query advertising 1232 has limit %d", got)
	}

	// A client advertising less than 512 is still given 512, as the
	// specification says.
	opt2 := opt
	if err := opt2.Header.SetEDNS0(100, dnsmessage.RCodeSuccess, false); err != nil {
		t.Fatal(err)
	}
	withEDNS.Additionals = []dnsmessage.Resource{opt2}
	raw, _ = withEDNS.Pack()
	if got := udpLimit(raw); got != 512 {
		t.Errorf("a tiny advertised size gave limit %d, want 512", got)
	}

	if got := udpLimit([]byte("garbage")); got != 512 {
		t.Errorf("garbage gave limit %d", got)
	}
}

func TestTruncateLeavesSmallRepliesAlone(t *testing.T) {
	r := newRig(t)
	r.both("site.com", "142.250.1.1")
	out := r.srv.Handle(context.Background(), packQuery(t, "site.com", dnsmessage.TypeA))

	if got := truncate(out, 512); string(got) != string(out) {
		t.Error("a reply that fits was altered")
	}
	short := truncate(out, 20)
	var msg dnsmessage.Message
	if err := msg.Unpack(short); err != nil {
		t.Fatal(err)
	}
	if !msg.Truncated || len(msg.Answers) != 0 || len(msg.Questions) != 1 {
		t.Errorf("truncated form = %+v", msg)
	}
}

func TestServeReturnsCleanlyWhenTheContextEnds(t *testing.T) {
	r := newRig(t)
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.srv.Serve(ctx, udp, tcp) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v after the context ended, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestIdleTCPConnectionsAreClosed(t *testing.T) {
	// Not waiting out the real timeout: just confirm a connection that sends
	// a zero-length frame is dropped instead of spinning the server.
	r := newRig(t)
	l := startLive(t, r.srv, r)

	conn, err := net.Dial("tcp", l.tcpAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Error("the server kept a connection that sent an empty frame")
	}
}

func TestConcurrentQueriesAgree(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 20; i++ {
		r.both(hostName(i), netip.AddrFrom4([4]byte{8, 8, byte(i), 1}).String())
	}
	l := startLive(t, r.srv, r)

	var wg sync.WaitGroup
	results := make([][]string, 100)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := exchangeUDP(t, l.udpAddr(), packQuery(t, hostName(i%20), dnsmessage.TypeA))
			results[i] = answerAddrs(msg)
		}()
	}
	wg.Wait()

	// Every query for one site, however concurrent, got the same stand-in.
	byHost := map[int]string{}
	for i, got := range results {
		if len(got) != 1 {
			t.Fatalf("query %d got %v", i, got)
		}
		if prev, ok := byHost[i%20]; ok && prev != got[0] {
			t.Errorf("%s got both %s and %s", hostName(i%20), prev, got[0])
		}
		byHost[i%20] = got[0]
	}
	if r.mapper.adds() != 20 {
		t.Errorf("%d translations were installed, want exactly 20", r.mapper.adds())
	}
	if st := r.srv.Stats(); st.PoolSize != 20 {
		t.Errorf("pool holds %d, want 20", st.PoolSize)
	}
}

func hostName(i int) string { return "host" + string(rune('a'+i)) + ".example" }
