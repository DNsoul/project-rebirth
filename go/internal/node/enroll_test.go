package node

import (
	"bytes"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"awgrotator/internal/awg"
	"awgrotator/internal/proto"
)

// enrollConfig is a configuration with the enrollment peer and registration
// switched on, the way the fleet package builds it.
func enrollConfig(t *testing.T) *Config {
	t.Helper()
	cfg := validConfig(t)
	enrollPeer, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Peers = append(cfg.Peers, Peer{PublicKey: enrollPeer.Public, AllowedIPs: []string{"10.8.0.251/32"}})
	cfg.Enroll = &EnrollConfig{
		Listen:      DefaultEnrollListen,
		PeerAddress: DefaultEnrollPeerAddress,
		ContentKey:  proto.Encode(bytes.Repeat([]byte{7}, proto.ContentKeySize)),
	}
	return cfg
}

func TestEnrollConfigPasses(t *testing.T) {
	if err := enrollConfig(t).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestEnrollIsOptional(t *testing.T) {
	cfg := validConfig(t)
	if cfg.Enroll != nil {
		t.Fatal("setup: a plain configuration already has registration")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if guard := cfg.Enroll.ForwardGuard(); guard != "" {
		t.Errorf("a server without registration got firewall rules for it:\n%s", guard)
	}
}

func TestEnrollConfigCatchesMistakes(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*Config)
		want   string
	}{
		{"listening somewhere else", func(c *Config) { c.Enroll.Listen = "0.0.0.0:7843" }, "tunnel address"},
		{"listening on the public side", func(c *Config) { c.Enroll.Listen = "203.0.113.9:7843" }, "tunnel address"},
		{"no port", func(c *Config) { c.Enroll.Listen = "10.8.0.1" }, "listen"},
		{"peer address in the device range", func(c *Config) { c.Enroll.PeerAddress = "10.8.0.40" }, "device range"},
		{"peer address outside the tunnel", func(c *Config) { c.Enroll.PeerAddress = "192.168.0.5" }, "inside"},
		{"peer address is the server", func(c *Config) { c.Enroll.PeerAddress = "10.8.0.1" }, "device range"},
		{"peer address nonsense", func(c *Config) { c.Enroll.PeerAddress = "nope" }, "peer address"},
		{"nobody holds the peer address", func(c *Config) { c.Peers[1].AllowedIPs = []string{"10.8.0.252/32"} }, "no peer"},
		{"short content key", func(c *Config) { c.Enroll.ContentKey = proto.Encode([]byte("short")) }, "content key"},
		{"no content key", func(c *Config) { c.Enroll.ContentKey = "" }, "content key"},
		{"negative cap", func(c *Config) { c.Enroll.MaxDevices = -1 }, "negative"},
	}
	for _, tc := range cases {
		cfg := enrollConfig(t)
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

// Registration listening anywhere but inside the tunnel would show the
// service to the internet.
func TestRegistrationCannotBeExposedByAMisconfiguredAddress(t *testing.T) {
	cfg := enrollConfig(t)
	cfg.Address = "10.9.0.1/24"
	cfg.Subnet = "10.9.0.0/24"
	cfg.Peers[1].AllowedIPs = []string{"10.9.0.251/32"}
	cfg.Enroll.PeerAddress = "10.9.0.251"
	// Listen still says 10.8.0.1, which is no longer the server's address.
	if err := cfg.Validate(); err == nil {
		t.Fatal("registration was allowed to listen on an address the tunnel does not have")
	}
}

func TestEnrollSurvivesSaveAndLoad(t *testing.T) {
	cfg := enrollConfig(t)
	cfg.Enroll.Approval = true
	cfg.Enroll.Preapproved = []string{proto.Encode(bytes.Repeat([]byte{9}, proto.TunnelKeySize))}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enroll == nil || !reflect.DeepEqual(*got.Enroll, *cfg.Enroll) {
		t.Errorf("registration settings did not survive: %+v", got.Enroll)
	}
}

func TestABadPreapprovedKeyIsRefused(t *testing.T) {
	cfg := enrollConfig(t)
	cfg.Enroll.Preapproved = []string{"short"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a preapproved key that is not a key was accepted")
	}
}

func TestRegistryPathDefaults(t *testing.T) {
	ec := enrollConfig(t).Enroll
	if got := ec.Registry(); !strings.HasSuffix(got, "registry.json") {
		t.Errorf("default registry path = %q", got)
	}
	ec.RegistryPath = "/tmp/elsewhere.json"
	if ec.Registry() != "/tmp/elsewhere.json" {
		t.Error("an explicit path was not used")
	}
}

// The enrollment key is published to everyone who has the app, so the server
// must not forward what comes in through it.
func TestTheEnrollmentPeerIsNotForwarded(t *testing.T) {
	guard := enrollConfig(t).Enroll.ForwardGuard()
	for _, want := range []string{"hook forward", "priority -10", "ip saddr 10.8.0.251 drop"} {
		if !strings.Contains(guard, want) {
			t.Errorf("the guard is missing %q:\n%s", want, guard)
		}
	}
	// It has to be decided before the firewall that ufw keeps gets a say, and
	// it must only ever name the enrollment peer.
	if strings.Count(guard, "ip saddr") != 1 {
		t.Errorf("the guard names more than the enrollment peer:\n%s", guard)
	}
}

func TestDevicePoolLeavesTheReservedAddressesAlone(t *testing.T) {
	pool, err := enrollConfig(t).Pool()
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"10.8.0.1": false, "10.8.0.2": true, "10.8.0.249": true,
		"10.8.0.250": false, "10.8.0.251": false, "10.8.1.5": false,
	} {
		a, err := netip.ParseAddr(host)
		if err != nil {
			t.Fatal(err)
		}
		if got := pool.Contains(a); got != want {
			t.Errorf("pool contains %s = %v, want %v", host, got, want)
		}
	}
}
