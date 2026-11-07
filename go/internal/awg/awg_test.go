package awg

import (
	"strings"
	"testing"
)

func TestGenerateKeypairDerivesItsPublicHalf(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if kp.Private.IsZero() || kp.Public.IsZero() {
		t.Fatal("a key came out zero")
	}

	got, err := PublicFrom(kp.Private)
	if err != nil {
		t.Fatalf("PublicFrom: %v", err)
	}
	if got != kp.Public {
		t.Error("the derived public key does not match the generated one")
	}
}

func TestPrivateKeyIsClamped(t *testing.T) {
	for i := 0; i < 20; i++ {
		kp, err := GenerateKeypair()
		if err != nil {
			t.Fatalf("GenerateKeypair: %v", err)
		}
		p := kp.Private
		if p[0]&7 != 0 {
			t.Errorf("low three bits of byte 0 are set: %08b", p[0])
		}
		if p[31]&128 != 0 {
			t.Errorf("high bit of byte 31 is set: %08b", p[31])
		}
		if p[31]&64 == 0 {
			t.Errorf("bit 6 of byte 31 is clear: %08b", p[31])
		}
	}
}

func TestKeyEncodings(t *testing.T) {
	kp, err := GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if got := len(kp.Public.Hex()); got != 2*KeySize {
		t.Errorf("hex length = %d, want %d", got, 2*KeySize)
	}
	back, err := ParseKeyBase64(kp.Public.Base64())
	if err != nil {
		t.Fatalf("ParseKeyBase64: %v", err)
	}
	if back != kp.Public {
		t.Error("base64 round trip changed the key")
	}
	if _, err := ParseKeyBase64("not base64!"); err == nil {
		t.Error("garbage parsed as a key")
	}
	if _, err := ParseKeyBase64("c2hvcnQ="); err == nil {
		t.Error("a short key was accepted")
	}
}

func TestGenerateParamsAlwaysValid(t *testing.T) {
	for i := 0; i < 50; i++ {
		p, err := GenerateParams()
		if err != nil {
			t.Fatalf("GenerateParams: %v", err)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("generated invalid parameters: %v (%+v)", err, p)
		}
		for name, v := range map[string]int{"S1": p.S1, "S2": p.S2, "S3": p.S3, "S4": p.S4} {
			if v < HeaderNonceSize {
				t.Errorf("%s = %d, below the header protection minimum %d", name, v, HeaderNonceSize)
			}
		}
		if p.Jmin > p.Jmax {
			t.Errorf("Jmin %d > Jmax %d", p.Jmin, p.Jmax)
		}
		if p.HeaderProtectionKey.IsZero() {
			t.Error("no header protection key was generated")
		}
		if !p.RandomTrailers {
			t.Error("RandomTrailers is off")
		}
		if !strings.HasPrefix(p.I1, "<r ") {
			t.Errorf("I1 = %q", p.I1)
		}
		// S4 rides on every data packet, so it must stay small enough to
		// leave room inside the MTU.
		if p.S4 > 64 {
			t.Errorf("S4 = %d, too large for per-packet overhead", p.S4)
		}
	}
}

func TestGenerateParamsVariesBetweenGenerations(t *testing.T) {
	a, err := GenerateParams()
	if err != nil {
		t.Fatalf("GenerateParams: %v", err)
	}
	b, err := GenerateParams()
	if err != nil {
		t.Fatalf("GenerateParams: %v", err)
	}
	if a.HeaderProtectionKey == b.HeaderProtectionKey {
		t.Fatal("two generations got the same header protection key")
	}
}

func TestValidateRejectsShortJunkWithHeaderProtection(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	p := Params{
		Jc: 4, Jmin: 50, Jmax: 500,
		S1: HeaderNonceSize, S2: HeaderNonceSize, S3: HeaderNonceSize,
		S4:                  HeaderNonceSize - 1,
		HeaderProtectionKey: key,
	}
	err = p.Validate()
	if err == nil {
		t.Fatal("S4 below the nonce size was accepted")
	}
	if !strings.Contains(err.Error(), "S4") {
		t.Errorf("error does not name S4: %v", err)
	}
}

func TestValidateAllowsShortJunkWithoutHeaderProtection(t *testing.T) {
	p := Params{Jc: 1, Jmin: 10, Jmax: 20, S1: 5, S2: 5, S3: 5, S4: 5}
	if err := p.Validate(); err != nil {
		t.Errorf("without a header protection key the minimum should not apply: %v", err)
	}
}

func TestValidateRejectsInvertedJunkRange(t *testing.T) {
	p := Params{Jmin: 500, Jmax: 100}
	if err := p.Validate(); err == nil {
		t.Fatal("Jmin greater than Jmax was accepted")
	}
}

func TestRandomPortAvoidsTheWireGuardDefault(t *testing.T) {
	for i := 0; i < 100; i++ {
		port, err := RandomPort()
		if err != nil {
			t.Fatalf("RandomPort: %v", err)
		}
		if port < portRange.min || port > portRange.max {
			t.Fatalf("port %d outside %d-%d", port, portRange.min, portRange.max)
		}
		if port == 51820 {
			t.Fatal("drew the WireGuard default port")
		}
	}
}

func testServerConfig(t *testing.T) (ServerConfig, Keypair, Keypair) {
	t.Helper()
	server, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	client, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	params, err := GenerateParams()
	if err != nil {
		t.Fatal(err)
	}
	return ServerConfig{
		PrivateKey: server.Private,
		ListenPort: 41820,
		Params:     params,
		Peers: []Peer{{
			PublicKey:  client.Public,
			AllowedIPs: []string{"10.8.0.2/32"},
		}},
	}, server, client
}

func TestUAPIUsesHexAndLeavesHeadersAlone(t *testing.T) {
	cfg, server, client := testServerConfig(t)
	got := cfg.UAPI()

	for _, want := range []string{
		"private_key=" + server.Private.Hex(),
		"listen_port=41820",
		"header_protection_key=" + cfg.Params.HeaderProtectionKey.Hex(),
		"random_trailers=true",
		"replace_peers=true",
		"public_key=" + client.Public.Hex(),
		"allowed_ip=10.8.0.2/32",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("UAPI is missing %q\n%s", want, got)
		}
	}

	// 3.1 protects headers with a cipher, so the 2.x magic header knobs must
	// stay at their defaults.
	for _, unwanted := range []string{"h1=", "h2=", "h3=", "h4="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("UAPI sets %q, which must be left default under header protection", unwanted)
		}
	}
	// Base64 must not leak into the socket form.
	if strings.Contains(got, server.Private.Base64()) {
		t.Error("UAPI carries a base64 key; the socket form is hex")
	}
}

func TestClientConfUsesBase64AndDigits(t *testing.T) {
	cfg, server, client := testServerConfig(t)
	cc := ClientConfig{
		PrivateKey:      client.Private,
		Address:         "10.8.0.2/32",
		DNS:             []string{"1.1.1.1", "9.9.9.9"},
		Params:          cfg.Params,
		ServerPublicKey: server.Public,
		Endpoint:        "203.0.113.9:41820",
		AllowedIPs:      []string{"0.0.0.0/0"},
	}
	got := cc.Render()

	for _, want := range []string{
		"[Interface]",
		"PrivateKey = " + client.Private.Base64(),
		"Address = 10.8.0.2/32",
		"DNS = 1.1.1.1, 9.9.9.9",
		"MTU = 1280",
		"HeaderProtectionKey = " + cfg.Params.HeaderProtectionKey.Base64(),
		"RandomTrailers = 1",
		"[Peer]",
		"PublicKey = " + server.Public.Base64(),
		"AllowedIPs = 0.0.0.0/0",
		"Endpoint = 203.0.113.9:41820",
		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("client config is missing %q\n%s", want, got)
		}
	}

	// "on"/"off" is what amneziawg-go refuses, so it must never appear.
	if strings.Contains(got, " on") || strings.Contains(got, " off") {
		t.Errorf("client config uses on/off for a boolean\n%s", got)
	}
	// Hex must not leak into the file form.
	if strings.Contains(got, client.Private.Hex()) {
		t.Error("client config carries a hex key; the file form is base64")
	}
}

// The AmneziaWG client parses this file strictly: a byte order mark, a stray
// line outside a section or a missing " = " makes it reject the whole file
// with a message that points at the wrong thing. Guard the shape here.
func TestClientConfShapeIsStrictlyValid(t *testing.T) {
	cfg, server, client := testServerConfig(t)
	got := ClientConfig{
		PrivateKey:      client.Private,
		Address:         "10.8.0.2/32",
		DNS:             []string{"1.1.1.1", "9.9.9.9"},
		Params:          cfg.Params,
		ServerPublicKey: server.Public,
		Endpoint:        "203.0.113.9:41820",
		AllowedIPs:      []string{"0.0.0.0/0"},
	}.Render()

	if strings.HasPrefix(got, BOM) {
		t.Error("rendered config starts with a byte order mark")
	}
	if !strings.HasPrefix(got, "[Interface]\n") {
		t.Errorf("rendered config starts with %q", got[:min(24, len(got))])
	}
	if strings.ContainsRune(got, '\r') {
		t.Error("rendered config contains a carriage return")
	}

	section := ""
	for n, line := range strings.Split(got, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if section == "" {
			t.Fatalf("line %d sits outside any section: %q", n+1, line)
		}
		if !strings.Contains(line, " = ") {
			t.Errorf("line %d is not \"Key = value\": %q", n+1, line)
		}
		if strings.TrimSpace(strings.SplitN(line, " = ", 2)[1]) == "" {
			t.Errorf("line %d has an empty value: %q", n+1, line)
		}
	}
	if section != "[Peer]" {
		t.Errorf("last section is %q, want [Peer]", section)
	}
}

func TestClientConfOmitsEmptyFields(t *testing.T) {
	cc := ClientConfig{
		Address:    "10.8.0.2/32",
		AllowedIPs: []string{"0.0.0.0/0"},
		Params:     Params{Jc: 4, Jmin: 50, Jmax: 500, S1: 12, S2: 12, S3: 12, S4: 12},
	}
	got := cc.Render()
	if strings.Contains(got, "DNS") {
		t.Error("an empty DNS list produced a DNS line")
	}
	if strings.Contains(got, "HeaderProtectionKey") {
		t.Error("an unset header protection key produced a line")
	}
	if strings.Contains(got, "I1") {
		t.Error("an empty I1 produced a line")
	}
	if !strings.Contains(got, "RandomTrailers = 0\n") {
		t.Errorf("RandomTrailers should still be written as 0\n%s", got)
	}
}

func TestUAPIPeerEndpointAndKeepaliveAreOptional(t *testing.T) {
	cfg, _, client := testServerConfig(t)

	// A server's peer carries neither.
	plain := cfg.UAPI()
	if strings.Contains(plain, "endpoint=") || strings.Contains(plain, "persistent_keepalive_interval=") {
		t.Errorf("a peer without them still emitted lines:\n%s", plain)
	}

	// A client's peer needs both to find the server and keep the mapping open.
	cfg.Peers = []Peer{{
		PublicKey:  client.Public,
		AllowedIPs: []string{"0.0.0.0/0"},
		Endpoint:   "203.0.113.9:41820",
		Keepalive:  5,
	}}
	got := cfg.UAPI()
	for _, want := range []string{"endpoint=203.0.113.9:41820\n", "persistent_keepalive_interval=5\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the UAPI is missing %q:\n%s", want, got)
		}
	}

	// And amneziawg-go has to accept what we wrote.
	dev := newTestDevice(t)
	if err := dev.IpcSet(got); err != nil {
		t.Fatalf("amneziawg-go rejected a peer with endpoint and keepalive: %v", err)
	}
}
