package fleet

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
	"awgrotator/internal/node"
	"awgrotator/internal/proto"
)

var testContentKey = bytes.Repeat([]byte{0x42}, proto.ContentKeySize)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewGenerationIsValidAndComplete(t *testing.T) {
	g, err := NewGeneration(GenOptions{Split: true, ContentKey: testContentKey})
	if err != nil {
		t.Fatalf("NewGeneration: %v", err)
	}
	if err := g.Node.Validate(); err != nil {
		t.Fatalf("the node configuration is not valid: %v", err)
	}
	if g.Node.Split == nil {
		t.Error("split was requested but the node configuration has none")
	}
	if len(g.Node.Peers) != 2 {
		t.Fatalf("%d peers, want the enrollment peer and the test peer", len(g.Node.Peers))
	}
	if g.Node.Peers[0].PublicKey != g.Enroll.Public || g.Node.Peers[1].PublicKey != g.Health.Public {
		t.Error("the peers are not the generated enrollment and test keys")
	}
	if g.Node.ListenPort != g.Port || g.Node.PrivateKey != g.Server.Private || g.Node.Params != g.Params {
		t.Error("the node configuration does not carry the generation's own key, port and parameters")
	}
}

func TestPlainGenerationHasNoSplit(t *testing.T) {
	g, err := NewGeneration(GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if g.Node.Split != nil {
		t.Error("a generation made without split has a split section")
	}
}

// Devices register themselves, so the server has to be set up to take them, and
// has to be told the key their requests are sealed with.
func TestTheServerIsSetUpToTakeDevices(t *testing.T) {
	g, err := NewGeneration(GenOptions{ContentKey: testContentKey})
	if err != nil {
		t.Fatal(err)
	}
	ec := g.Node.Enroll
	if ec == nil {
		t.Fatal("no registration section although a content key was given")
	}
	if ec.ContentKey != proto.Encode(testContentKey) {
		t.Error("the server was given a different content key")
	}
	if ec.PeerAddress+"/32" != EnrollPeer {
		t.Errorf("the registration address %s is not the enrollment peer's %s", ec.PeerAddress, EnrollPeer)
	}
	if ec.Listen != enroll.ServerAddr.String() {
		t.Errorf("listens on %s, devices will ask %s", ec.Listen, enroll.ServerAddr)
	}

	plain, err := NewGeneration(GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Node.Enroll != nil {
		t.Error("a registration section without a content key could never read a request")
	}
}

// The addresses reserved for the health check and for enrollment must never be
// handed to a device.
func TestReservedAddressesAreOutsideTheDevicePool(t *testing.T) {
	g, err := NewGeneration(GenOptions{ContentKey: testContentKey})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := g.Node.Pool()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range g.Node.Peers {
		for _, cidr := range p.AllowedIPs {
			addr := strings.SplitN(cidr, "/", 2)[0]
			if pool.Contains(mustAddr(t, addr)) {
				t.Errorf("%s is reserved for %q but a device could be given it", addr, p.Note)
			}
		}
	}
	if pool.Contains(mustAddr(t, "10.8.0.1")) {
		t.Error("a device could be given the server's own address")
	}
}

// Two servers must not share anything that identifies them on the wire.
func TestGenerationsDoNotShareKeysOrParameters(t *testing.T) {
	a, err := NewGeneration(GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewGeneration(GenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Server.Private == b.Server.Private || a.Enroll.Private == b.Enroll.Private || a.Health.Private == b.Health.Private {
		t.Error("two generations share a key")
	}
	if a.Params.HeaderProtectionKey == b.Params.HeaderProtectionKey {
		t.Error("two generations share a header protection key")
	}
	// The three keys inside one generation must also differ from each other.
	if a.Server.Private == a.Enroll.Private || a.Enroll.Private == a.Health.Private || a.Server.Private == a.Health.Private {
		t.Error("keys within one generation are identical")
	}
}

func TestGenerationParamsOverrideIsValidated(t *testing.T) {
	good := awg.Params{Jc: 3, Jmin: 50, Jmax: 500, S1: 20, S2: 20, S3: 20, S4: 20}
	g, err := NewGeneration(GenOptions{Params: &good})
	if err != nil {
		t.Fatalf("a valid override was refused: %v", err)
	}
	if g.Params != good {
		t.Errorf("the override was not used: %+v", g.Params)
	}

	bad := awg.Params{Jmin: 900, Jmax: 100}
	if _, err := NewGeneration(GenOptions{Params: &bad}); err == nil {
		t.Error("an invalid override was accepted")
	}
}

func TestBodyDescribesTheGeneration(t *testing.T) {
	g, err := NewGeneration(GenOptions{Split: true, ContentKey: testContentKey})
	if err != nil {
		t.Fatal(err)
	}
	split := ClientNet{DNS: []string{"198.18.0.1"}, AllowedIPs: []string{"198.18.0.0/15", "91.108.4.0/22"}}
	body := g.Body("sto", "203.0.113.9", split, time.Unix(1_800_000_000, 0))

	if err := body.Validate(); err != nil {
		t.Fatalf("the body is not valid: %v", err)
	}
	if !strings.HasPrefix(body.Endpoint, "203.0.113.9:") || body.Region != "sto" {
		t.Errorf("endpoint %q region %q", body.Endpoint, body.Region)
	}
	if body.ServerPublicKey != g.Server.Public {
		t.Error("the body carries the wrong server key")
	}
	if strings.Join(body.DNS, ",") != "198.18.0.1" || strings.Join(body.AllowedIPs, ",") != "198.18.0.0/15,91.108.4.0/22" {
		t.Errorf("routes: dns %v, allowed %v", body.DNS, body.AllowedIPs)
	}

	// A device registers through the enrollment peer, so that is what the
	// body hands out, and nothing else's private key.
	if body.Enroll == nil || body.Enroll.PrivateKey != g.Enroll.Private {
		t.Fatal("the body does not carry the enrollment peer's key")
	}
	if body.Enroll.Address != EnrollPeer || body.Enroll.Server != enroll.ServerAddr.String() {
		t.Errorf("enrollment info = %+v", body.Enroll)
	}
	if body.Shared != nil {
		t.Error("the body still carries a key shared by every client")
	}

	raw, err := body.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string]string{
		"the test peer's private key": g.Health.Private.Base64(),
		"the server's private key":    g.Server.Private.Base64(),
	} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("%s leaked into the published body", name)
		}
	}
}

// What the node is told and what devices are told must describe the same
// enrollment peer, or nobody could register.
func TestEnrollmentPeerAgreesBetweenNodeAndBody(t *testing.T) {
	g, err := NewGeneration(GenOptions{ContentKey: testContentKey})
	if err != nil {
		t.Fatal(err)
	}
	body := g.Body("sto", "203.0.113.9", FullTunnel, time.Unix(1_800_000_000, 0))

	pub, err := awg.PublicFrom(body.Enroll.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range g.Node.Peers {
		if p.PublicKey == pub {
			found = true
			if len(p.AllowedIPs) != 1 || p.AllowedIPs[0] != body.Enroll.Address {
				t.Errorf("the node allows %v for it, the body says %s", p.AllowedIPs, body.Enroll.Address)
			}
		}
	}
	if !found {
		t.Error("the key in the body does not belong to any peer the node accepts")
	}
	if body.Enroll.Server != node.DefaultEnrollListen {
		t.Errorf("devices ask %s, the node listens on %s", body.Enroll.Server, node.DefaultEnrollListen)
	}
}

func TestFullTunnelClientNet(t *testing.T) {
	if len(FullTunnel.AllowedIPs) != 1 || FullTunnel.AllowedIPs[0] != "0.0.0.0/0" {
		t.Errorf("FullTunnel routes = %v", FullTunnel.AllowedIPs)
	}
	if len(FullTunnel.DNS) == 0 {
		t.Error("FullTunnel has no DNS")
	}
}
