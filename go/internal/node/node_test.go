package node

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/awg"
)

func validConfig(t *testing.T) *Config {
	t.Helper()
	server, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	client, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	params, err := awg.GenerateParams()
	if err != nil {
		t.Fatal(err)
	}
	return &Config{
		Version:    ConfigVersion,
		Interface:  "awg0",
		ListenPort: 41820,
		PrivateKey: server.Private,
		Params:     params,
		Address:    "10.8.0.1/24",
		Subnet:     "10.8.0.0/24",
		Peers: []Peer{{
			PublicKey:  client.Public,
			AllowedIPs: []string{"10.8.0.2/32"},
		}},
	}
}

func TestValidConfigPasses(t *testing.T) {
	if err := validConfig(t).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateCatchesBadConfigs(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*Config)
		want   string
	}{
		{"wrong version", func(c *Config) { c.Version = 99 }, "version"},
		{"empty interface", func(c *Config) { c.Interface = "" }, "interface"},
		{"interface with a slash", func(c *Config) { c.Interface = "awg/0" }, "interface"},
		{"interface too long", func(c *Config) { c.Interface = "averyveryverylongname" }, "interface"},
		{"port zero", func(c *Config) { c.ListenPort = 0 }, "port"},
		{"port too high", func(c *Config) { c.ListenPort = 70000 }, "port"},
		{"no private key", func(c *Config) { c.PrivateKey = awg.Key{} }, "private key"},
		{"address without prefix", func(c *Config) { c.Address = "10.8.0.1" }, "address"},
		{"nonsense subnet", func(c *Config) { c.Subnet = "not a subnet" }, "subnet"},
		{"no peers", func(c *Config) { c.Peers = nil }, "peers"},
		{"peer without key", func(c *Config) { c.Peers[0].PublicKey = awg.Key{} }, "public key"},
		{"peer without addresses", func(c *Config) { c.Peers[0].AllowedIPs = nil }, "allowed"},
		{"peer with bad address", func(c *Config) { c.Peers[0].AllowedIPs = []string{"10.8.0.2"} }, "allowed"},
		{"junk range inverted", func(c *Config) { c.Params.Jmin, c.Params.Jmax = 900, 100 }, "Jmin"},
		{"junk prefix too short", func(c *Config) { c.Params.S1 = 1 }, "S1"},
	}
	for _, tc := range cases {
		cfg := validConfig(t)
		tc.break_(cfg)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

func TestSaveLoadConfig(t *testing.T) {
	cfg := validConfig(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.PrivateKey != cfg.PrivateKey {
		t.Error("private key did not survive the round trip")
	}
	if got.Params != cfg.Params {
		t.Errorf("parameters changed:\n got %+v\nwant %+v", got.Params, cfg.Params)
	}
	if got.Peers[0].PublicKey != cfg.Peers[0].PublicKey {
		t.Error("peer key did not survive the round trip")
	}
}

// Keys must be stored in their readable base64 form, not as an array of 32
// numbers.
func TestConfigStoresKeysAsBase64(t *testing.T) {
	cfg := validConfig(t)
	raw, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"private_key": "`+cfg.PrivateKey.Base64()+`"`) {
		t.Errorf("private key is not stored as base64:\n%s", text)
	}
	if strings.Contains(text, "[") && strings.Contains(text, "255,") {
		t.Error("a key leaked out as a numeric array")
	}
}

func TestServerConfigCarriesPeers(t *testing.T) {
	cfg := validConfig(t)
	sc := cfg.ServerConfig()
	if sc.PrivateKey != cfg.PrivateKey || sc.ListenPort != cfg.ListenPort {
		t.Error("server configuration lost the interface details")
	}
	if len(sc.Peers) != 1 || sc.Peers[0].PublicKey != cfg.Peers[0].PublicKey {
		t.Error("server configuration lost the peer")
	}
	if !strings.Contains(sc.UAPI(), "allowed_ip=10.8.0.2/32\n") {
		t.Errorf("UAPI is missing the peer address:\n%s", sc.UAPI())
	}
}

func TestStatusPathDefaults(t *testing.T) {
	cfg := validConfig(t)
	if got := cfg.Status(); got != DefaultStatusPath {
		t.Errorf("Status() = %q, want the default", got)
	}
	cfg.StatusPath = "/tmp/elsewhere.json"
	if got := cfg.Status(); got != "/tmp/elsewhere.json" {
		t.Errorf("Status() = %q", got)
	}
}

// A realistic IpcGet reply, including the private key that must not be
// carried into the status report.
const sampleUAPI = `private_key=a0b1c2d3e4f5061728394a5b6c7d8e9f00112233445566778899aabbccddeeff
listen_port=41820
jc=4
public_key=1111111111111111111111111111111111111111111111111111111111111111
preshared_key=0000000000000000000000000000000000000000000000000000000000000000
protocol_version=1
endpoint=203.0.113.44:51234
last_handshake_time_sec=1762400000
last_handshake_time_nsec=123
tx_bytes=4096
rx_bytes=8192
persistent_keepalive_interval=25
allowed_ip=10.8.0.2/32
public_key=2222222222222222222222222222222222222222222222222222222222222222
last_handshake_time_sec=0
tx_bytes=0
rx_bytes=0
allowed_ip=10.8.0.3/32
errno=0
`

func TestParseDeviceStatus(t *testing.T) {
	port, peers, err := ParseDeviceStatus(sampleUAPI)
	if err != nil {
		t.Fatalf("ParseDeviceStatus: %v", err)
	}
	if port != 41820 {
		t.Errorf("port = %d", port)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(peers))
	}

	first := peers[0]
	if first.Endpoint != "203.0.113.44:51234" {
		t.Errorf("endpoint = %q", first.Endpoint)
	}
	if first.RxBytes != 8192 || first.TxBytes != 4096 {
		t.Errorf("counters = rx %d tx %d", first.RxBytes, first.TxBytes)
	}
	if first.LastHandshake.Unix() != 1762400000 {
		t.Errorf("handshake = %v", first.LastHandshake)
	}
	// Hex on the wire, base64 in the report.
	if strings.Contains(first.PublicKey, "1111111111") {
		t.Errorf("public key was left in hex: %q", first.PublicKey)
	}
	if peers[1].LastHandshake != (time.Time{}) {
		t.Errorf("a peer that never handshook got a time: %v", peers[1].LastHandshake)
	}

	// The private key must not appear anywhere in the report.
	for _, p := range peers {
		if strings.Contains(p.PublicKey, "a0b1c2") || strings.Contains(p.Endpoint, "a0b1c2") {
			t.Error("the private key leaked into the status report")
		}
	}
}

func TestConnectedUsesTheFreshnessWindow(t *testing.T) {
	now := time.Unix(1762400000, 0)
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"just now", now, true},
		{"a minute ago", now.Add(-time.Minute), true},
		{"ten minutes ago", now.Add(-10 * time.Minute), false},
		{"never", time.Time{}, false},
	}
	for _, tc := range cases {
		p := PeerStatus{LastHandshake: tc.at}
		if got := p.Connected(now); got != tc.want {
			t.Errorf("%s: Connected = %v, want %v", tc.name, got, tc.want)
		}
	}

	st := Status{UpdatedAt: now, Peers: []PeerStatus{
		{LastHandshake: time.Time{}},
		{LastHandshake: now.Add(-time.Minute)},
	}}
	if !st.AnyConnected() {
		t.Error("AnyConnected missed a live peer")
	}
}

func TestParseStatusRoundTrip(t *testing.T) {
	raw := []byte(`{"interface":"awg0","listen_port":41820,"peers":[{"public_key":"x","rx_bytes":5}]}`)
	st, err := ParseStatus(raw)
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if st.Interface != "awg0" || st.ListenPort != 41820 || len(st.Peers) != 1 {
		t.Errorf("got %+v", st)
	}
	if _, err := ParseStatus([]byte("not json")); err == nil {
		t.Error("garbage parsed as a status")
	}
}

func TestSplitConfigDefaultsAreValid(t *testing.T) {
	cfg := validConfig(t)
	cfg.Split = NewSplitConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the default split configuration was refused: %v", err)
	}
}

func TestSplitConfigIsOptional(t *testing.T) {
	cfg := validConfig(t)
	if cfg.Split != nil {
		t.Fatal("a plain configuration should have no split section")
	}
	raw, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "split") {
		t.Errorf("a configuration without split mentions it:\n%s", raw)
	}
}

func TestSplitConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*SplitConfig)
		want   string
	}{
		{"bad range", func(s *SplitConfig) { s.StandInRange = "nonsense" }, "range"},
		{"bad resolver address", func(s *SplitConfig) { s.DNSAddress = "nope" }, "resolver address"},
		{"resolver outside the range", func(s *SplitConfig) { s.DNSAddress = "10.0.0.1" }, "outside"},
		{"bad client subnet", func(s *SplitConfig) { s.ClientSubnet = "77.88.8.0" }, "client subnet"},
		{"upstream without a port", func(s *SplitConfig) { s.PrimaryUpstream = "8.8.8.8" }, "primary upstream"},
		{"upstream by name", func(s *SplitConfig) { s.SecondaryUpstream = "dns.example:53" }, "secondary upstream"},
	}
	for _, tc := range cases {
		cfg := validConfig(t)
		cfg.Split = NewSplitConfig()
		tc.break_(cfg.Split)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}
}

// If the stand-in range overlapped the tunnel's own subnet, a stand-in would
// be indistinguishable from a client address.
func TestSplitRangeMustNotOverlapTheTunnelSubnet(t *testing.T) {
	cfg := validConfig(t)
	cfg.Split = NewSplitConfig()
	cfg.Split.StandInRange = "10.8.0.0/16"
	cfg.Split.DNSAddress = "10.8.0.200"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("overlapping ranges were accepted")
	}
	if !strings.Contains(err.Error(), "overlaps") {
		t.Errorf("error does not say why: %v", err)
	}
}

func TestSplitConfigSurvivesSaveAndLoad(t *testing.T) {
	cfg := validConfig(t)
	cfg.Split = NewSplitConfig()
	cfg.Split.ClientSubnet = "95.24.0.0/24"

	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Split == nil || *got.Split != *cfg.Split {
		t.Errorf("split section changed:\n got %+v\nwant %+v", got.Split, cfg.Split)
	}
}

// The status is published. It must say how the resolver is doing without ever
// saying which sites were looked up.
func TestSplitStatusCarriesOnlyCounts(t *testing.T) {
	st := Status{Split: &SplitStatus{RURanges: 7413, ForceTunnel: 5}}
	st.Split.Queries, st.Split.Tunnel, st.Split.PoolSize = 100, 40, 12

	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	split, ok := generic["split"].(map[string]any)
	if !ok {
		t.Fatalf("no split section in %s", raw)
	}
	for key, value := range split {
		if _, isNumber := value.(float64); !isNumber {
			t.Errorf("split.%s is a %T, want a plain count", key, value)
		}
	}
	for _, want := range []string{"queries", "direct", "tunnel", "filtered", "failures", "pool_size", "ru_ranges"} {
		if _, ok := split[want]; !ok {
			t.Errorf("split.%s is missing", want)
		}
	}
}

func TestStatusWithoutSplitOmitsTheSection(t *testing.T) {
	raw, err := json.Marshal(Status{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "split") {
		t.Errorf("a status without split mentions it: %s", raw)
	}
}
