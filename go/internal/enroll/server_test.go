package enroll

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/proto"
)

// fakeTunnel records the peers a server was asked to accept.
type fakeTunnel struct {
	mu      sync.Mutex
	peers   map[awg.Key]string
	setErr  error
	removed []awg.Key
}

func (f *fakeTunnel) SetPeer(pub awg.Key, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	if f.peers == nil {
		f.peers = map[awg.Key]string{}
	}
	f.peers[pub] = ip
	return nil
}

func (f *fakeTunnel) RemovePeer(pub awg.Key) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.peers, pub)
	f.removed = append(f.removed, pub)
	return nil
}

func (f *fakeTunnel) has(keyByte byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	var k awg.Key
	for i := range k {
		k[i] = keyByte
	}
	_, ok := f.peers[k]
	return ok
}

var (
	testKey  = bytes.Repeat([]byte{0x04}, proto.ContentKeySize)
	peerAddr = netip.MustParseAddr("10.8.0.251")
)

type rig struct {
	srv    *Server
	tunnel *fakeTunnel
	reg    *Registry
	now    time.Time
}

func newRig(t *testing.T, opts Options) *rig {
	t.Helper()
	r := &rig{
		tunnel: &fakeTunnel{},
		reg:    NewRegistry(filepath.Join(t.TempDir(), "r.json"), testPool(t), opts),
		now:    time.Unix(1_700_000_000, 0),
	}
	r.srv = &Server{
		Registry: r.reg, Peers: r.tunnel, ContentKey: testKey, From: peerAddr,
		Now: func() time.Time { return r.now },
	}
	return r
}

// ask sends one request and returns the opened reply, or nil if the server
// stayed silent.
func (r *rig) ask(t *testing.T, id, key byte, name string) *proto.EnrollResponse {
	t.Helper()
	req := request(id, key, name, r.now)
	blob, err := req.Seal(testKey)
	if err != nil {
		t.Fatal(err)
	}
	out := r.srv.Handle(peerAddr, []byte(blob))
	if out == nil {
		return nil
	}
	reply, err := proto.OpenEnrollResponse(testKey, string(out), req.Nonce)
	if err != nil {
		t.Fatalf("the reply does not open: %v", err)
	}
	return reply
}

func TestAnEnrolledDeviceIsAcceptedByTheTunnel(t *testing.T) {
	r := newRig(t, Options{})
	reply := r.ask(t, 1, 0x11, "windows")
	if reply == nil || reply.Error != "" || reply.Address != "10.8.0.2/32" {
		t.Fatalf("reply = %+v", reply)
	}
	if !r.tunnel.has(0x11) {
		t.Fatal("the key was not put into the tunnel")
	}
	if r.reg.Len() != 1 {
		t.Errorf("%d devices recorded", r.reg.Len())
	}
}

func TestTwoDevicesGetDifferentAddresses(t *testing.T) {
	r := newRig(t, Options{})
	a, b := r.ask(t, 1, 0x11, "windows"), r.ask(t, 2, 0x22, "phone")
	if a.Address == b.Address || !r.tunnel.has(0x11) || !r.tunnel.has(0x22) {
		t.Fatalf("a = %+v, b = %+v", a, b)
	}
}

func TestReinstallingReplacesTheKeyAndKeepsTheAddress(t *testing.T) {
	r := newRig(t, Options{})
	first := r.ask(t, 1, 0x11, "windows")
	second := r.ask(t, 1, 0x99, "windows") // same device, new key

	if second.Address != first.Address {
		t.Errorf("address moved from %s to %s", first.Address, second.Address)
	}
	if r.tunnel.has(0x11) {
		t.Error("the old key is still accepted")
	}
	if !r.tunnel.has(0x99) {
		t.Error("the new key is not accepted")
	}
	if r.reg.Len() != 1 {
		t.Errorf("%d devices after a reinstall", r.reg.Len())
	}
}

func TestOnlyTheEnrollmentPeerIsAnswered(t *testing.T) {
	r := newRig(t, Options{})
	req := request(1, 0x11, "windows", r.now)
	blob, _ := req.Seal(testKey)
	for _, from := range []string{"10.8.0.2", "10.8.0.250", "127.0.0.1", "10.8.0.1"} {
		if out := r.srv.Handle(netip.MustParseAddr(from), []byte(blob)); out != nil {
			t.Errorf("answered %s", from)
		}
	}
	if r.reg.Len() != 0 {
		t.Error("a device was registered from the wrong address")
	}
}

func TestSilenceForWhoeverLacksTheKey(t *testing.T) {
	r := newRig(t, Options{})
	req := request(1, 0x11, "windows", r.now)
	wrong, _ := req.Seal(bytes.Repeat([]byte{0x77}, proto.ContentKeySize))
	for name, datagram := range map[string][]byte{
		"wrong key": []byte(wrong), "garbage": []byte("hello"), "empty": nil, "binary": {0, 1, 2, 3},
	} {
		if out := r.srv.Handle(peerAddr, datagram); out != nil {
			t.Errorf("%s: answered", name)
		}
	}
	if r.reg.Len() != 0 {
		t.Error("something was registered")
	}
}

func TestAStaleRequestGetsNoAnswer(t *testing.T) {
	r := newRig(t, Options{})
	req := request(1, 0x11, "windows", r.now.Add(-time.Hour))
	blob, _ := req.Seal(testKey)
	if out := r.srv.Handle(peerAddr, []byte(blob)); out != nil {
		t.Error("answered a request an hour old")
	}
}

func TestRefusalsReachTheDevice(t *testing.T) {
	r := newRig(t, Options{MaxDevices: 1})
	r.ask(t, 1, 0x11, "windows")

	if got := r.ask(t, 2, 0x22, "phone"); got == nil || got.Error != proto.EnrollFull {
		t.Errorf("full: %+v", got)
	}
	if r.tunnel.has(0x22) {
		t.Error("a refused device was let into the tunnel")
	}
	if got := r.ask(t, 3, 0x11, "clone"); got == nil || got.Error != proto.EnrollKeyInUse {
		t.Errorf("key in use: %+v", got)
	}
}

func TestTooManyRequestsAreAskedToWait(t *testing.T) {
	r := newRig(t, Options{})
	r.srv.Burst, r.srv.Every = 3, time.Second

	var busy int
	for i := 0; i < 6; i++ {
		got := r.ask(t, 1, 0x11, "windows") // the same request again and again
		if got != nil && got.Error == proto.EnrollBusy {
			busy++
		}
	}
	if busy != 3 {
		t.Errorf("%d of 6 were told to wait, want 3", busy)
	}

	r.now = r.now.Add(2 * time.Second)
	if got := r.ask(t, 1, 0x11, "windows"); got == nil || got.Error != "" {
		t.Errorf("still busy after the bucket refilled: %+v", got)
	}
}

func TestATunnelThatWillNotTakeThePeerLeavesNoRecord(t *testing.T) {
	r := newRig(t, Options{})
	r.tunnel.setErr = errors.New("device is closed")
	if got := r.ask(t, 1, 0x11, "windows"); got == nil || got.Error != proto.EnrollBusy {
		t.Fatalf("reply = %+v", got)
	}
	if r.reg.Len() != 0 {
		t.Error("the device was recorded although the tunnel does not accept it")
	}
}

func TestARecordThatCannotBeSavedTakesThePeerBackOut(t *testing.T) {
	r := newRig(t, Options{})
	dir := filepath.Dir(r.reg.path)
	if err := os.Mkdir(filepath.Join(dir, "r.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := r.ask(t, 1, 0x11, "windows"); got == nil || got.Error != proto.EnrollBusy {
		t.Fatalf("reply = %+v", got)
	}
	if r.tunnel.has(0x11) {
		t.Error("the peer stayed in the tunnel although it was not recorded")
	}
}

func TestEvictingAStaleDeviceRemovesItsKey(t *testing.T) {
	r := newRig(t, Options{MaxDevices: 1, StaleAfter: 24 * time.Hour})
	r.ask(t, 1, 0x11, "old")
	r.now = r.now.Add(48 * time.Hour)

	got := r.ask(t, 2, 0x22, "new")
	if got == nil || got.Error != "" || got.Address != "10.8.0.2/32" {
		t.Fatalf("reply = %+v", got)
	}
	if r.tunnel.has(0x11) || !r.tunnel.has(0x22) {
		t.Error("the tunnel does not match the registry after the swap")
	}
}

// ---- over a real socket

func listen(t *testing.T) net.PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback UDP: %v", err)
	}
	return pc
}

func dialUDP(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// lossy drops the first n datagrams it reads, like a tunnel losing the first
// packets after a handshake.
type lossy struct {
	net.PacketConn
	mu   sync.Mutex
	drop int
}

func (l *lossy) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := l.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		l.mu.Lock()
		skip := l.drop > 0
		if skip {
			l.drop--
		}
		l.mu.Unlock()
		if !skip {
			return n, addr, nil
		}
	}
}

func serve(t *testing.T, r *rig, pc net.PacketConn) netip.AddrPort {
	t.Helper()
	r.srv.From = netip.MustParseAddr("127.0.0.1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.srv.Serve(ctx, pc) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return pc.LocalAddr().(*net.UDPAddr).AddrPort()
}

func deviceRequest(id, key byte, server netip.AddrPort, now time.Time) Request {
	return Request{
		DeviceID:   bytes.Repeat([]byte{id}, proto.DeviceIDSize),
		PublicKey:  bytes.Repeat([]byte{key}, proto.TunnelKeySize),
		Name:       "test",
		ContentKey: testKey,
		Server:     server,
		Now:        func() time.Time { return now },
	}
}

func TestEnrollOverUDP(t *testing.T) {
	r := newRig(t, Options{})
	server := serve(t, r, listen(t))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addr, err := Enroll(ctx, dialUDP, deviceRequest(1, 0x11, server, r.now))
	if err != nil {
		t.Fatal(err)
	}
	if addr != "10.8.0.2/32" || !r.tunnel.has(0x11) {
		t.Errorf("address %q, in tunnel %v", addr, r.tunnel.has(0x11))
	}
}

func TestEnrollRetriesWhenTheFirstPacketsAreLost(t *testing.T) {
	r := newRig(t, Options{})
	server := serve(t, r, &lossy{PacketConn: listen(t), drop: 2})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	addr, err := Enroll(ctx, dialUDP, deviceRequest(1, 0x11, server, r.now))
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" || time.Since(started) < time.Second {
		t.Errorf("address %q after %s: it should have taken at least one retry", addr, time.Since(started))
	}
}

func TestEnrollGivesUpWhenNobodyAnswers(t *testing.T) {
	pc := listen(t) // nothing reads from it
	defer pc.Close()
	server := pc.LocalAddr().(*net.UDPAddr).AddrPort()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err := Enroll(ctx, dialUDP, deviceRequest(1, 0x11, server, time.Now()))
	if err == nil {
		t.Fatal("enrolled with nobody listening")
	}
}

func TestEnrollReportsARefusal(t *testing.T) {
	r := newRig(t, Options{MaxDevices: 1})
	server := serve(t, r, listen(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := Enroll(ctx, dialUDP, deviceRequest(1, 0x11, server, r.now)); err != nil {
		t.Fatal(err)
	}
	_, err := Enroll(ctx, dialUDP, deviceRequest(2, 0x22, server, r.now))
	var refused *Refused
	if !errors.As(err, &refused) || refused.Reason != proto.EnrollFull {
		t.Fatalf("err = %v, want a refusal for being full", err)
	}
}

func TestEnrollWaitsOutABusyServer(t *testing.T) {
	r := newRig(t, Options{})
	r.srv.Burst, r.srv.Every = 1, 2*time.Second
	// Use up the bucket with a different request, so the real one is told to
	// wait and has to come back.
	r.ask(t, 9, 0x99, "other")

	// The server's clock is real here, so the bucket refills while we wait.
	r.srv.Now = nil
	r.srv.bucket = bucket{}
	r.srv.allow(time.Now())
	server := serve(t, r, listen(t))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	addr, err := Enroll(ctx, dialUDP, deviceRequest(1, 0x11, server, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if addr == "" || time.Since(started) < time.Second {
		t.Errorf("address %q after %s: the busy answer should have cost a pause", addr, time.Since(started))
	}
}
