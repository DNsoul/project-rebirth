package awglib

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"awgrotator/internal/awg"
)

const confVectorsPath = "../../../spec/conf-vectors.json"

// The files the apps write are the ones this reads, so every vector the Dart
// port has to match is also read here and written back by the Go renderer: if
// the three ever disagree, a client would be handed a file it cannot use.
func TestEveryVectorReadsAndWritesBackUnchanged(t *testing.T) {
	raw, err := os.ReadFile(confVectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []struct {
			Name string `json:"name"`
			Conf string `json:"conf"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("no vectors")
	}
	for _, c := range v.Cases {
		s, err := ParseConf(c.Conf)
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		var dns []string
		for _, d := range s.DNS {
			dns = append(dns, d.String())
		}
		var allowed []string
		for _, p := range s.AllowedIPs {
			allowed = append(allowed, p.String())
		}
		got := awg.ClientConfig{
			PrivateKey: s.PrivateKey, Address: s.Addresses[0].String(), DNS: dns, MTU: s.MTU,
			Params: s.Params, ServerPublicKey: s.ServerPublic, Endpoint: s.Endpoint.String(),
			AllowedIPs: allowed, Keepalive: s.Keepalive,
		}.Render()
		if got != c.Conf {
			t.Errorf("%s: written back differently\n--- read\n%s\n--- written\n%s", c.Name, c.Conf, got)
		}
	}
}

func TestAFileWithABOMOrWindowsLineEndingsStillReads(t *testing.T) {
	raw, _ := os.ReadFile(confVectorsPath)
	var v struct {
		Cases []struct{ Conf string } `json:"cases"`
	}
	_ = json.Unmarshal(raw, &v)
	conf := v.Cases[0].Conf
	if _, err := ParseConf("\ufeff" + strings.ReplaceAll(conf, "\n", "\r\n")); err != nil {
		t.Errorf("BOM and CRLF: %v", err)
	}
}

func goodConf(t *testing.T) string {
	t.Helper()
	kp, _ := awg.GenerateKeypair()
	sp, _ := awg.GenerateKeypair()
	return awg.ClientConfig{
		PrivateKey: kp.Private, Address: "10.8.0.7/32", DNS: []string{"198.18.0.1"},
		Params:          awg.Params{Jc: 3, Jmin: 40, Jmax: 80, S1: 20, S2: 20, S3: 20, S4: 20},
		ServerPublicKey: sp.Public, Endpoint: "203.0.113.9:41820",
		AllowedIPs: []string{"198.18.0.0/15"},
	}.Render()
}

func TestAMisspeltSettingIsAnErrorNotSilence(t *testing.T) {
	good := goodConf(t)
	if _, err := ParseConf(good); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(good, "Jmin = 40", "Jmim = 40", 1)
	if _, err := ParseConf(bad); err == nil || !strings.Contains(err.Error(), "jmim") {
		t.Errorf("a misspelt setting was accepted or the error does not name it: %v", err)
	}
}

func TestMissingPiecesAreNamed(t *testing.T) {
	good := goodConf(t)
	for name, tc := range map[string]struct{ drop, want string }{
		"no key":      {"PrivateKey", "PrivateKey"},
		"no address":  {"Address", "Address"},
		"no endpoint": {"Endpoint", "Endpoint"},
		"no networks": {"AllowedIPs", "AllowedIPs"},
		"no server":   {"PublicKey", "PublicKey"},
	} {
		var kept []string
		for _, l := range strings.Split(good, "\n") {
			if !strings.HasPrefix(l, tc.drop+" =") {
				kept = append(kept, l)
			}
		}
		_, err := ParseConf(strings.Join(kept, "\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTwoPeersOrNoSectionOrAJunkLineAreRefused(t *testing.T) {
	good := goodConf(t)
	for name, conf := range map[string]string{
		"two peers":     good + "\n[Peer]\nPublicKey = AAAA\n",
		"outside":       "PrivateKey = x\n" + good,
		"a junk line":   good + "\nthis is not a setting\n",
		"unknown block": good + "\n[Wat]\n",
		"a bad number":  strings.Replace(good, "Jc = 3", "Jc = three", 1),
		"a bad network": strings.Replace(good, "198.18.0.0/15", "not-a-net", 1),
	} {
		if _, err := ParseConf(conf); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBareAddressesAreHostsAndTheDefaultMTUIs1280(t *testing.T) {
	good := strings.Replace(goodConf(t), "Address = 10.8.0.7/32", "Address = 10.8.0.7", 1)
	good = strings.Replace(good, "MTU = 1280\n", "", 1)
	s, err := ParseConf(good)
	if err != nil {
		t.Fatal(err)
	}
	if s.Addresses[0].String() != "10.8.0.7/32" || s.MTU != awg.MTU {
		t.Errorf("address %v, mtu %d", s.Addresses, s.MTU)
	}
}

func TestUAPICarriesTheEndpointKeepaliveAndAllowedIPs(t *testing.T) {
	s, err := ParseConf(goodConf(t))
	if err != nil {
		t.Fatal(err)
	}
	u := s.UAPI()
	for _, want := range []string{
		"endpoint=203.0.113.9:41820", "persistent_keepalive_interval=25",
		"allowed_ip=198.18.0.0/15", "jc=3", "replace_peers=true",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("the device settings lack %q:\n%s", want, u)
		}
	}
}

func TestHandshakeTimeIsTheNewestAndZeroMeansNone(t *testing.T) {
	if _, ok := parseHandshake("public_key=aa\nlast_handshake_time_sec=0\nlast_handshake_time_nsec=0\n"); ok {
		t.Error("zero was taken as a handshake")
	}
	got, ok := parseHandshake("last_handshake_time_sec=100\nlast_handshake_time_sec=300\nlast_handshake_time_sec=200\n")
	if !ok || got.Unix() != 300 {
		t.Errorf("got %v %v", got, ok)
	}
}

// ------------------------------------------------------------ a real tunnel

// A client brought up by Up talks to a real server device over loopback: the
// handshake completes, data crosses, and Close really closes. The server runs
// the same library as the production nodes.
func TestARunningClientReachesAServerThroughTheRealProtocol(t *testing.T) {
	serverKP, _ := awg.GenerateKeypair()
	clientKP, _ := awg.GenerateKeypair()
	params, err := awg.GenerateParams()
	if err != nil {
		t.Fatal(err)
	}
	port := freeUDPPort(t)

	// The server.
	serverTun, serverNet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.8.0.1")}, nil, awg.MTU)
	if err != nil {
		t.Fatal(err)
	}
	server := device.NewDevice(serverTun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
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
	ln, err := serverNet.ListenTCPAddrPort(netip.MustParseAddrPort("10.8.0.1:8080"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "pong from the server")
			c.Close()
		}
	}()

	// The client, started the way the app starts it: from a configuration file.
	conf := awg.ClientConfig{
		PrivateKey: clientKP.Private, Address: "10.8.0.7/32", DNS: []string{"10.8.0.1"},
		Params: params, ServerPublicKey: serverKP.Public,
		Endpoint: fmt.Sprintf("127.0.0.1:%d", port), AllowedIPs: []string{"10.8.0.0/24"}, Keepalive: 25,
	}.Render()
	settings, err := ParseConf(conf)
	if err != nil {
		t.Fatal(err)
	}
	clientTun, clientNet, err := netstack.CreateNetTUN(addrsOf(settings.Addresses), settings.DNS, settings.MTU)
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	tunnel, err := Up(clientTun, settings, &Logger{Verbosef: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := tunnel.LatestHandshake(); ok {
		t.Error("a handshake before anything was sent")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := clientNet.DialContext(ctx, "tcp", "10.8.0.1:8080")
	if err != nil {
		t.Fatalf("dial through the tunnel: %v", err)
	}
	reply, _ := io.ReadAll(c)
	c.Close()
	if string(reply) != "pong from the server" {
		t.Errorf("reply = %q", reply)
	}

	when, ok := tunnel.LatestHandshake()
	if !ok || time.Since(when) > time.Minute {
		t.Errorf("handshake %v %v", when, ok)
	}
	if v4, _ := tunnel.SocketFDs(); v4 < 0 {
		t.Errorf("no socket reported for the client (v4=%d)", v4)
	}

	tunnel.Close()
	tunnel.Close() // harmless the second time
	if _, ok := tunnel.LatestHandshake(); ok {
		t.Error("a closed tunnel still reports a handshake")
	}
}

func TestAWrongServerKeyNeverHandshakes(t *testing.T) {
	serverKP, _ := awg.GenerateKeypair()
	clientKP, _ := awg.GenerateKeypair()
	stranger, _ := awg.GenerateKeypair()
	params, _ := awg.GenerateParams()
	port := freeUDPPort(t)

	serverTun, _, _ := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr("10.8.0.1")}, nil, awg.MTU)
	server := device.NewDevice(serverTun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	t.Cleanup(server.Close)
	_ = server.IpcSet(awg.ServerConfig{PrivateKey: serverKP.Private, ListenPort: port, Params: params,
		Peers: []awg.Peer{{PublicKey: clientKP.Public, AllowedIPs: []string{"10.8.0.7/32"}}}}.UAPI())
	_ = server.Up()

	conf := awg.ClientConfig{
		PrivateKey: clientKP.Private, Address: "10.8.0.7/32", Params: params,
		ServerPublicKey: stranger.Public, // not the server's key
		Endpoint:        fmt.Sprintf("127.0.0.1:%d", port), AllowedIPs: []string{"10.8.0.0/24"},
	}.Render()
	settings, err := ParseConf(conf)
	if err != nil {
		t.Fatal(err)
	}
	clientTun, clientNet, _ := netstack.CreateNetTUN(addrsOf(settings.Addresses), nil, settings.MTU)
	tunnel, err := Up(clientTun, settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if c, err := clientNet.DialContext(ctx, "tcp", "10.8.0.1:8080"); err == nil {
		c.Close()
		t.Fatal("a connection got through with the wrong key")
	}
	if _, ok := tunnel.LatestHandshake(); ok {
		t.Error("a handshake with the wrong key")
	}
}

func addrsOf(ps []netip.Prefix) []netip.Addr {
	var out []netip.Addr
	for _, p := range ps {
		out = append(out, p.Addr())
	}
	return out
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}
