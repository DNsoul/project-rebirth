package tunneltest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	"golang.org/x/net/dns/dnsmessage"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
)

// These tests put two real AmneziaWG 3.1 devices in one process and let them
// talk over loopback: a server that answers DNS and serves a TLS site, and the
// client under test. That exercises the real handshake with the real
// obfuscation parameters, which is the part a unit test of our own code could
// never reach.

var (
	resolverAddr = netip.MustParseAddrPort("198.18.0.1:53")
	standInAddr  = netip.MustParseAddr("198.18.0.5")
	standInRange = netip.MustParsePrefix("198.18.0.0/15")
)

type serverOptions struct {
	// answers maps a name to the address the resolver returns for it.
	answers map[string]string

	// noSite leaves the stand-in address with nothing listening on it.
	noSite bool

	// params overrides the generated obfuscation parameters.
	params *awg.Params

	// registrar starts the device registration service on the server, as the
	// node does. The enrollment peer is always accepted; without this it has
	// nobody to talk to.
	registrar bool
}

type rig struct {
	target Target

	// reg describes how a device registers on this server, and registry is the
	// server's list of devices. The registry is nil unless the registrar runs.
	reg      Registration
	registry *enroll.Registry
}

func startServer(t *testing.T, so serverOptions) rig {
	t.Helper()

	serverKP, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	clientKP, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	enrollKP, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	params, err := awg.GenerateParams()
	if err != nil {
		t.Fatal(err)
	}
	if so.params != nil {
		params = *so.params
	}

	// Find a free UDP port for the server's tunnel endpoint.
	probe, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	clientAddr := netip.MustParseAddr("10.8.0.250")
	tunDev, tnet, err := netstack.CreateNetTUN(
		[]netip.Addr{netip.MustParseAddr("10.8.0.1"), resolverAddr.Addr(), standInAddr},
		nil, awg.MTU)
	if err != nil {
		t.Fatalf("server network: %v", err)
	}
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	t.Cleanup(dev.Close)

	uapi := awg.ServerConfig{
		PrivateKey: serverKP.Private,
		ListenPort: port,
		Params:     params,
		Peers: []awg.Peer{
			{PublicKey: clientKP.Public, AllowedIPs: []string{"10.8.0.250/32"}},
			{PublicKey: enrollKP.Public, AllowedIPs: []string{"10.8.0.251/32"}},
		},
	}.UAPI()
	if err := dev.IpcSet(uapi); err != nil {
		t.Fatalf("configure the server device: %v", err)
	}
	if err := dev.Up(); err != nil {
		t.Fatalf("bring the server device up: %v", err)
	}

	// The resolver inside the tunnel.
	pc, err := tnet.ListenUDPAddrPort(resolverAddr)
	if err != nil {
		t.Fatalf("listen for DNS: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	go serveDNS(pc, so.answers)

	// The "site" the stand-in address leads to.
	if !so.noSite {
		ln, err := tnet.ListenTCPAddrPort(netip.AddrPortFrom(standInAddr, 443))
		if err != nil {
			t.Fatalf("listen for TLS: %v", err)
		}
		t.Cleanup(func() { ln.Close() })
		go serveTLS(ln)
	}

	endpoint := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
	r := rig{
		target: Target{
			Endpoint:      endpoint,
			ServerPublic:  serverKP.Public,
			Params:        params,
			ClientPrivate: clientKP.Private,
			ClientAddr:    clientAddr,
			Resolver:      resolverAddr,
			StandIn:       standInRange,
		},
		reg: Registration{
			Endpoint:     endpoint,
			ServerPublic: serverKP.Public,
			Params:       params,
			PeerPrivate:  enrollKP.Private,
			PeerAddr:     netip.MustParseAddr("10.8.0.251"),
			ContentKey:   e2eContentKey,
		},
	}

	// The registration service, answering inside the tunnel as it does on a
	// real server.
	if so.registrar {
		pool, err := enroll.NewPool("10.8.0.0/24", 2, 249)
		if err != nil {
			t.Fatal(err)
		}
		r.registry = enroll.NewRegistry(filepath.Join(t.TempDir(), "registry.json"), pool, enroll.Options{})
		srv := &enroll.Server{
			Registry:   r.registry,
			Peers:      deviceSink{dev},
			ContentKey: e2eContentKey,
			From:       netip.MustParseAddr("10.8.0.251"),
			Burst:      100,
		}
		regConn, err := tnet.ListenUDPAddrPort(enroll.ServerAddr)
		if err != nil {
			t.Fatalf("listen for registrations: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = srv.Serve(ctx, regConn) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	return r
}

func serveDNS(pc net.PacketConn, answers map[string]string) {
	buf := make([]byte, 1500)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		var req dnsmessage.Message
		if err := req.Unpack(buf[:n]); err != nil || len(req.Questions) != 1 {
			continue
		}
		q := req.Questions[0]
		resp := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: req.ID, Response: true, RecursionAvailable: true},
			Questions: req.Questions,
		}
		name := strings.TrimSuffix(q.Name.String(), ".")
		if addr, ok := answers[name]; ok && q.Type == dnsmessage.TypeA {
			resp.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AResource{A: netip.MustParseAddr(addr).As4()},
			}}
		}
		if out, err := resp.Pack(); err == nil {
			_, _ = pc.WriteTo(out, from)
		}
	}
}

func serveTLS(ln net.Listener) {
	cert := selfSigned()
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			tc := tls.Server(c, cfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			buf := make([]byte, 512)
			if _, err := tc.Read(buf); err != nil {
				return
			}
			_, _ = tc.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		}()
	}
}

func selfSigned() tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, _ := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func find(rep Report, name string) (Check, bool) {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func TestAHealthyServerPasses(t *testing.T) {
	r := startServer(t, serverOptions{answers: map[string]string{
		"one.one.one.one": "198.18.0.5",
		"yandex.ru":       "5.255.255.77",
	}})

	rep, err := Run(context.Background(), r.target, Options{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("report:\n%s", rep)
	if !rep.Passed() {
		t.Fatalf("a healthy server did not pass:\n%s", rep)
	}
	for _, name := range []string{
		"handshake and resolver", "foreign name is tunnelled",
		"site reachable through the stand-in", "local name goes direct",
	} {
		c, ok := find(rep, name)
		if !ok || !c.OK {
			t.Errorf("check %q: found=%v ok=%v\n%s", name, ok, c.OK, rep)
		}
	}
	if c, _ := find(rep, "site reachable through the stand-in"); !strings.Contains(c.Detail, "HTTP/1.1 200") {
		t.Errorf("the site check does not show the reply: %q", c.Detail)
	}
}

func TestAServerOnTheWrongKeyNeverHandshakes(t *testing.T) {
	r := startServer(t, serverOptions{answers: map[string]string{"one.one.one.one": "198.18.0.5"}})

	// Test it as if the successor had been given a different key than the one
	// that was published.
	other, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	r.target.ServerPublic = other.Public

	start := time.Now()
	rep, err := Run(context.Background(), r.target, Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Passed() {
		t.Fatalf("a server with the wrong key passed:\n%s", rep)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("took %s to give up, the timeout was 5s", elapsed)
	}
	c, ok := find(rep, "handshake and resolver")
	if !ok || c.OK {
		t.Errorf("the handshake check should have failed: %+v", c)
	}
	if _, ran := find(rep, "site reachable through the stand-in"); ran {
		t.Error("later checks ran although the tunnel never came up")
	}
}

func TestAServerWithDifferentParametersNeverHandshakes(t *testing.T) {
	r := startServer(t, serverOptions{answers: map[string]string{"one.one.one.one": "198.18.0.5"}})

	// The obfuscation parameters have to match on both ends. This is the mistake
	// a successor built from the wrong generation would make.
	wrong, err := awg.GenerateParams()
	if err != nil {
		t.Fatal(err)
	}
	wrong.S1 = r.target.Params.S1 + 1
	r.target.Params = wrong

	rep, err := Run(context.Background(), r.target, Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed() {
		t.Fatalf("a server with mismatched parameters passed:\n%s", rep)
	}
}

func TestAForeignNameWithARealAddressFails(t *testing.T) {
	r := startServer(t, serverOptions{answers: map[string]string{
		"one.one.one.one": "1.1.1.1", // the resolver forgot to substitute
	}})

	rep, err := Run(context.Background(), r.target, Options{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed() {
		t.Fatalf("a foreign site that was given its real address passed:\n%s", rep)
	}
	c, ok := find(rep, "foreign name is tunnelled")
	if !ok || c.OK || !strings.Contains(c.Detail, "not a stand-in") {
		t.Errorf("check = %+v", c)
	}
}

func TestAStandInWithNothingBehindItFails(t *testing.T) {
	r := startServer(t, serverOptions{
		answers: map[string]string{"one.one.one.one": "198.18.0.5"},
		noSite:  true,
	})

	rep, err := Run(context.Background(), r.target, Options{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed() {
		t.Fatalf("a stand-in with no translation behind it passed:\n%s", rep)
	}
	if c, ok := find(rep, "site reachable through the stand-in"); !ok || c.OK {
		t.Errorf("the site check should have failed: %+v", c)
	}
}

// The local-name check is informational: a resolver showing a different view
// of a site must not by itself condemn a server.
func TestTheLocalNameCheckIsNotRequired(t *testing.T) {
	r := startServer(t, serverOptions{answers: map[string]string{
		"one.one.one.one": "198.18.0.5",
		"yandex.ru":       "198.18.0.5", // tunnelled, where a local client would expect direct
	}})

	rep, err := Run(context.Background(), r.target, Options{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed() {
		t.Fatalf("an informational check failed the run:\n%s", rep)
	}
	if c, _ := find(rep, "local name goes direct"); c.OK || c.Required {
		t.Errorf("local check = %+v, want a failed, non-required note", c)
	}
}

func TestEmptyReportDoesNotPass(t *testing.T) {
	if (Report{}).Passed() {
		t.Error("a report with no checks passed")
	}
	if (Report{Checks: []Check{{Name: "x", OK: true}}}).Passed() {
		t.Error("a report with no required checks passed")
	}
}

func TestReportString(t *testing.T) {
	rep := Report{Checks: []Check{
		{Name: "a", OK: true, Required: true, Detail: "fine"},
		{Name: "b", Required: true, Detail: "broken"},
		{Name: "c", Detail: "meh"},
	}, Elapsed: 1500 * time.Millisecond}
	out := rep.String()
	for _, want := range []string{"[ok  ] a: fine", "[FAIL] b: broken", "[note] c: meh", "1.5s"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report is missing %q:\n%s", want, out)
		}
	}
}
