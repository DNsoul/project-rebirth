package node

import (
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"awgrotator/internal/awg"
	"awgrotator/internal/awglib"
)

const keyA = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789+/abcde="

func TestTheAbbreviationIsTheLibrarysOwn(t *testing.T) {
	if got := abbreviateKey(keyA); got != "AbCd…bcde" {
		t.Fatalf("got %q", got)
	}
	if got := trimPeerName("peer(AbCd…bcde)"); got != "AbCd…bcde" {
		t.Fatalf("got %q", got)
	}
}

func TestAClientThatFollowsUpIsNotStuck(t *testing.T) {
	var w HandshakeWatch
	t0 := time.Unix(1_000_000, 500_000_000)
	w.Initiation("peer(AbCd…bcde)", t0)
	// The handshake completes a fraction of a second later, in the same
	// whole second as the library reports it.
	peers := []PeerStatus{{PublicKey: keyA, LastHandshake: t0.Truncate(time.Second)}}
	st := w.Observe(peers, t0.Add(time.Minute))
	if st.Answered != 1 || st.Stuck != 0 || peers[0].HandshakeTries != 0 {
		t.Fatalf("%+v %+v", st, peers[0])
	}
}

func TestAClientThatKeepsKnockingIsStuckAfterTheGrace(t *testing.T) {
	var w HandshakeWatch
	t0 := time.Unix(1_000_000, 0)
	old := t0.Add(-time.Hour)
	for i := 0; i < 10; i++ {
		w.Initiation("peer(AbCd…bcde)", t0.Add(time.Duration(i)*5*time.Second))
	}
	peers := func() []PeerStatus { return []PeerStatus{{PublicKey: keyA, LastHandshake: old}} }

	p := peers()
	if st := w.Observe(p, t0.Add(30*time.Second)); st.Stuck != 0 {
		t.Fatalf("stuck after 30 seconds: %+v", st)
	}
	p = peers()
	st := w.Observe(p, t0.Add(50*time.Second))
	if st.Stuck != 1 || p[0].HandshakeTries != 10 || st.Answered != 10 {
		t.Fatalf("after 50 seconds: %+v %+v", st, p[0])
	}

	// One that gave up long ago says nothing about now.
	p = peers()
	if st := w.Observe(p, t0.Add(45*time.Second+HandshakeFresh+time.Minute)); st.Stuck != 0 || p[0].HandshakeTries != 0 {
		t.Fatalf("still counted after it stopped knocking: %+v", st)
	}

	// Getting in clears it.
	w.Initiation("peer(AbCd…bcde)", t0.Add(10*time.Minute))
	p = []PeerStatus{{PublicKey: keyA, LastHandshake: t0.Add(10*time.Minute + time.Second)}}
	if st := w.Observe(p, t0.Add(12*time.Minute)); st.Stuck != 0 {
		t.Fatalf("stuck after getting in: %+v", st)
	}
}

func TestClientsThatStoppedKnockingAreForgotten(t *testing.T) {
	var w HandshakeWatch
	t0 := time.Unix(1_000_000, 0)
	w.Initiation("peer(Gone…zzzz)", t0)
	w.Observe(nil, t0.Add(2*time.Hour))
	if len(w.peers) != 0 {
		t.Fatalf("%d remembered", len(w.peers))
	}
}

// relay passes packets between a client and a server and can drop the
// server's answers, the way a path cut in one direction does.
type relay struct {
	pc         net.PacketConn
	server     *net.UDPAddr
	dropAnswer atomic.Bool
}

func newRelay(t *testing.T, serverPort int) *relay {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	r := &relay{pc: pc, server: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: serverPort}}
	var client atomic.Pointer[net.UDPAddr]
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			u := from.(*net.UDPAddr)
			if u.Port == serverPort {
				if c := client.Load(); c != nil && !r.dropAnswer.Load() {
					_, _ = pc.WriteTo(buf[:n], c)
				}
				continue
			}
			client.Store(u)
			_, _ = pc.WriteTo(buf[:n], r.server)
		}
	}()
	return r
}

func (r *relay) port() int { return r.pc.LocalAddr().(*net.UDPAddr).Port }

// TestTheWatchHearsTheRealLibrary runs a real server and client. It holds the
// log line the watch listens for to the library itself, and shows the two
// cases end to end: a client that gets in, and one whose answers are lost.
func TestTheWatchHearsTheRealLibrary(t *testing.T) {
	serverKP, _ := awg.GenerateKeypair()
	clientKP, _ := awg.GenerateKeypair()
	params, err := awg.GenerateParams()
	if err != nil {
		t.Fatal(err)
	}
	probe, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	var w HandshakeWatch
	var errs []string
	serverTun, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.8.0.1")}, nil, awg.MTU)
	if err != nil {
		t.Fatal(err)
	}
	server := device.NewDevice(serverTun, conn.NewDefaultBind(),
		w.Logger(func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }))
	t.Cleanup(server.Close)
	if err := server.IpcSet(awg.ServerConfig{
		PrivateKey: serverKP.Private, ListenPort: port, Params: params,
		Peers: []awg.Peer{{PublicKey: clientKP.Public, AllowedIPs: []string{"10.8.0.7/32"}}},
	}.UAPI()); err != nil {
		t.Fatal(err)
	}
	if err := server.Up(); err != nil {
		t.Fatal(err)
	}
	rl := newRelay(t, port)

	serverPeers := func() []PeerStatus {
		raw, err := server.IpcGet()
		if err != nil {
			t.Fatal(err)
		}
		_, peers, err := ParseDeviceStatus(raw)
		if err != nil {
			t.Fatal(err)
		}
		return peers
	}

	// A client whose answers never come back: the server keeps answering.
	rl.dropAnswer.Store(true)
	conf := awg.ClientConfig{
		PrivateKey: clientKP.Private, Address: "10.8.0.7/32",
		Params: params, ServerPublicKey: serverKP.Public,
		Endpoint: fmt.Sprintf("127.0.0.1:%d", rl.port()), AllowedIPs: []string{"10.8.0.0/24"}, Keepalive: 1,
	}.Render()
	settings, err := awglib.ParseConf(conf)
	if err != nil {
		t.Fatal(err)
	}
	clientTun, _, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.8.0.7")}, nil, settings.MTU)
	if err != nil {
		t.Fatal(err)
	}
	client, err := awglib.Up(clientTun, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	deadline := time.Now().Add(10 * time.Second)
	for w.Observe(serverPeers(), time.Now()).Answered == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the watch never heard an initiation; the library's log line may have changed (errors: %v)", errs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Looked at a minute on, the client counts as stuck: the server answered
	// it and never saw it follow up.
	peers := serverPeers()
	if st := w.Observe(peers, time.Now().Add(time.Minute)); st.Stuck != 1 || peers[0].HandshakeTries == 0 {
		t.Fatalf("a client whose answers were lost is not stuck: %+v %+v", st, peers[0])
	}

	// The path heals: the next attempt gets in and the client is clear.
	rl.dropAnswer.Store(false)
	deadline = time.Now().Add(15 * time.Second)
	for {
		if hs, ok := client.LatestHandshake(); ok && !hs.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the client never got in after the path healed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		peers := serverPeers()
		st := w.Observe(peers, time.Now())
		if st.Stuck == 0 && peers[0].HandshakeTries == 0 && !peers[0].LastHandshake.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still stuck after getting in: %+v %+v", st, peers[0])
		}
		time.Sleep(50 * time.Millisecond)
	}
}
