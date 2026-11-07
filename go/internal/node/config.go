// Package node is the server side: it brings up the AmneziaWG interface,
// turns the machine into a gateway, and reports on itself.
//
// amneziawg-go is embedded as a library rather than installed as a program,
// so a server needs nothing but this one static binary. That is what lets a
// replacement server be ready in under a minute.
package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
	"awgrotator/internal/proto"
)

// ConfigVersion is the on-disk layout version of the node configuration.
const ConfigVersion = 1

// DefaultConfigPath is where the node looks for its configuration.
const DefaultConfigPath = "/etc/awgnode/config.json"

// DefaultStatusPath is where the node writes what it knows about itself, for
// awgctl to read over SSH and, later, for the node's own status reports.
const DefaultStatusPath = "/run/awgnode/status.json"

// Peer is one client the server accepts.
type Peer struct {
	PublicKey  awg.Key  `json:"public_key"`
	AllowedIPs []string `json:"allowed_ips"`
	Note       string   `json:"note,omitempty"`
}

// Config is what awgnode reads at startup.
type Config struct {
	Version int `json:"version"`

	// Interface is the tunnel device name, e.g. "awg0".
	Interface string `json:"interface"`

	ListenPort int        `json:"listen_port"`
	PrivateKey awg.Key    `json:"private_key"`
	Params     awg.Params `json:"params"`

	// Address is the server's own address inside the tunnel, with prefix,
	// e.g. "10.8.0.1/24". Subnet is the whole tunnel range, which is what
	// gets masqueraded.
	Address string `json:"address"`
	Subnet  string `json:"subnet"`

	Peers []Peer `json:"peers"`

	// Split turns on the resolver that decides which sites go through the
	// tunnel. Nil leaves the server a plain full-tunnel gateway.
	Split *SplitConfig `json:"split,omitempty"`

	// Enroll turns on automatic registration of devices. Nil leaves the server
	// with only the peers listed above.
	Enroll *EnrollConfig `json:"enroll,omitempty"`

	// StatusPath overrides DefaultStatusPath.
	StatusPath string `json:"status_path,omitempty"`
}

// EnrollConfig describes how devices register themselves.
//
// The server cannot accept a device whose key it has never seen, so new devices
// reach it through one well-known peer, the enrollment peer, whose key is in
// the published configuration. That peer can talk to the registration service
// and nothing else: its traffic is not forwarded.
type EnrollConfig struct {
	// Listen is where registration is answered: the server's own tunnel
	// address and port.
	Listen string `json:"listen"`

	// PeerAddress is the enrollment peer's tunnel address. It is the only
	// source registration requests are accepted from, and the one source whose
	// forwarded traffic is dropped.
	PeerAddress string `json:"peer_address"`

	// ContentKey is what a request has to be sealed with (base64url).
	ContentKey string `json:"content_key"`

	// RegistryPath overrides where the device list is kept.
	RegistryPath string `json:"registry_path,omitempty"`

	// MaxDevices caps the device list; zero takes the default.
	MaxDevices int `json:"max_devices,omitempty"`
}

// Defaults for registration.
const (
	DefaultEnrollListen      = "10.8.0.1:7843" // enroll.ServerAddr
	DefaultEnrollPeerAddress = "10.8.0.251"
)

// ForwardGuard is the nftables chain that keeps the enrollment peer from being
// a way out to the internet. Its key is published to everyone who has the app,
// so what it may do has to be limited by the server, not by whoever holds it:
// the peer reaches the server's own registration service and nothing that
// would have to be forwarded.
//
// The chain hooks forward at priority -10, ahead of the chains ufw keeps, so a
// rule there cannot undo it. It is empty for a server without registration.
func (e *EnrollConfig) ForwardGuard() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf(`	chain enroll_guard {
		type filter hook forward priority -10; policy accept;
		ip saddr %s drop
	}
`, e.PeerAddress)
}

// Registry returns where the device list is kept.
func (e *EnrollConfig) Registry() string {
	if e.RegistryPath != "" {
		return e.RegistryPath
	}
	return enroll.DefaultRegistryPath
}

// Pool is the range of addresses devices are given. The rest of the tunnel
// subnet is reserved: .1 is the server, .250 the health check, .251 the
// enrollment peer.
func (c *Config) Pool() (enroll.Pool, error) {
	return enroll.NewPool(c.Subnet, 2, 249)
}

// validate checks the enrollment section against the rest of the config.
func (e *EnrollConfig) validate(c *Config) error {
	listen, err := netip.ParseAddrPort(e.Listen)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", e.Listen, err)
	}
	server, _, err := net.ParseCIDR(c.Address)
	if err != nil {
		return fmt.Errorf("address %q: %w", c.Address, err)
	}
	if got, ok := netip.AddrFromSlice(server); !ok || got.Unmap() != listen.Addr() {
		return fmt.Errorf("listen address %s is not the server's tunnel address %s: "+
			"registration must be reachable from inside the tunnel only", e.Listen, server)
	}

	peer, err := netip.ParseAddr(e.PeerAddress)
	if err != nil {
		return fmt.Errorf("enrollment peer address %q: %w", e.PeerAddress, err)
	}
	pool, err := c.Pool()
	if err != nil {
		return err
	}
	if !pool.Subnet.Contains(peer) || pool.Contains(peer) || peer == listen.Addr() {
		return fmt.Errorf("enrollment peer address %s must be inside %s but outside the device range", peer, c.Subnet)
	}
	found := false
	for _, p := range c.Peers {
		if slices.Contains(p.AllowedIPs, peer.String()+"/32") {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no peer is allowed %s/32: nothing could use the enrollment address", peer)
	}

	if _, err := proto.Decode(e.ContentKey, proto.ContentKeySize); err != nil {
		return fmt.Errorf("content key: %w", err)
	}
	if e.MaxDevices < 0 {
		return errors.New("max devices is negative")
	}
	return nil
}

// Paths of the files the resolver reads, on the server.
const (
	DefaultRUSetPath       = "/var/lib/awgnode/ru4.txt"
	DefaultForceTunnelPath = "/etc/awgnode/force_vpn.txt"
	DefaultForceDirectPath = "/etc/awgnode/force_direct.txt"
	DefaultPoolPath        = "/var/lib/awgnode/pool.json"
)

// SplitConfig describes the split-routing resolver.
type SplitConfig struct {
	// DNSAddress is where clients are told to send their queries. It lives
	// inside StandInRange and is added to the tunnel device.
	DNSAddress string `json:"dns_address"`

	// StandInRange is the block the stand-in addresses come from. The same
	// range and resolver address are used on every server, so an address a
	// client cached keeps meaning something after the server is replaced.
	StandInRange string `json:"stand_in_range"`

	RUSetPath       string `json:"ru_set_path,omitempty"`
	ForceTunnelPath string `json:"force_tunnel_path,omitempty"`
	ForceDirectPath string `json:"force_direct_path,omitempty"`
	PoolPath        string `json:"pool_path,omitempty"`

	// PrimaryUpstream is asked with ClientSubnet, SecondaryUpstream without.
	PrimaryUpstream   string `json:"primary_upstream"`
	SecondaryUpstream string `json:"secondary_upstream"`

	// ClientSubnet is the fixed local subnet sent to the primary upstream. It
	// is a stand-in: nothing about the real client is revealed.
	ClientSubnet string `json:"client_subnet"`
}

// Defaults for a split configuration.
const (
	DefaultStandInRange = "198.18.0.0/15"
	DefaultDNSAddress   = "198.18.0.1"
)

// NewSplitConfig returns a configuration with every default filled in.
func NewSplitConfig() *SplitConfig {
	return &SplitConfig{
		DNSAddress:        DefaultDNSAddress,
		StandInRange:      DefaultStandInRange,
		RUSetPath:         DefaultRUSetPath,
		ForceTunnelPath:   DefaultForceTunnelPath,
		ForceDirectPath:   DefaultForceDirectPath,
		PoolPath:          DefaultPoolPath,
		PrimaryUpstream:   "8.8.8.8:53",
		SecondaryUpstream: "1.1.1.1:53",
		ClientSubnet:      "77.88.8.0/24",
	}
}

// validate checks the split section.
func (s *SplitConfig) validate() error {
	rng, err := netip.ParsePrefix(s.StandInRange)
	if err != nil {
		return fmt.Errorf("stand-in range %q: %w", s.StandInRange, err)
	}
	dns, err := netip.ParseAddr(s.DNSAddress)
	if err != nil {
		return fmt.Errorf("resolver address %q: %w", s.DNSAddress, err)
	}
	if !rng.Contains(dns) {
		return fmt.Errorf("resolver address %s is outside the stand-in range %s", dns, rng)
	}
	if _, err := netip.ParsePrefix(s.ClientSubnet); err != nil {
		return fmt.Errorf("client subnet %q: %w", s.ClientSubnet, err)
	}
	for name, hostport := range map[string]string{
		"primary upstream": s.PrimaryUpstream, "secondary upstream": s.SecondaryUpstream,
	} {
		if _, err := netip.ParseAddrPort(hostport); err != nil {
			return fmt.Errorf("%s %q: %w", name, hostport, err)
		}
	}
	return nil
}

// interfaceName is what the kernel will accept as a device name.
var interfaceName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,14}$`)

// Validate checks the configuration before anything touches the system, so a
// typo fails at startup with a clear message instead of halfway through
// reconfiguring the network.
func (c *Config) Validate() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("configuration version %d, this build understands %d", c.Version, ConfigVersion)
	}
	if !interfaceName.MatchString(c.Interface) {
		return fmt.Errorf("interface name %q is not usable", c.Interface)
	}
	if c.ListenPort < 1 || c.ListenPort > 65535 {
		return fmt.Errorf("listen port %d is out of range", c.ListenPort)
	}
	if c.PrivateKey.IsZero() {
		return fmt.Errorf("no private key")
	}
	if err := c.Params.Validate(); err != nil {
		return fmt.Errorf("obfuscation parameters: %w", err)
	}
	if _, _, err := net.ParseCIDR(c.Address); err != nil {
		return fmt.Errorf("address %q: %w", c.Address, err)
	}
	_, tunnelNet, err := net.ParseCIDR(c.Subnet)
	if err != nil {
		return fmt.Errorf("subnet %q: %w", c.Subnet, err)
	}
	if c.Split != nil {
		if err := c.Split.validate(); err != nil {
			return fmt.Errorf("split: %w", err)
		}
		// Stand-ins and tunnel addresses sharing space would make a
		// stand-in indistinguishable from a client.
		rng, _ := netip.ParsePrefix(c.Split.StandInRange)
		if base, ok := netip.AddrFromSlice(tunnelNet.IP); ok && rng.Contains(base.Unmap()) {
			return fmt.Errorf("split: stand-in range %s overlaps the tunnel subnet %s", rng, c.Subnet)
		}
	}
	if len(c.Peers) == 0 {
		return fmt.Errorf("no peers: nothing would be able to connect")
	}
	for i, p := range c.Peers {
		if p.PublicKey.IsZero() {
			return fmt.Errorf("peer %d has no public key", i)
		}
		if len(p.AllowedIPs) == 0 {
			return fmt.Errorf("peer %d has no allowed addresses", i)
		}
		for _, cidr := range p.AllowedIPs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("peer %d allowed address %q: %w", i, cidr, err)
			}
		}
	}
	if c.Enroll != nil {
		if err := c.Enroll.validate(c); err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
	}
	return nil
}

// Status returns where this node writes its status.
func (c *Config) Status() string {
	if c.StatusPath != "" {
		return c.StatusPath
	}
	return DefaultStatusPath
}

// ServerConfig converts to the form the awg package renders from.
func (c *Config) ServerConfig() awg.ServerConfig {
	peers := make([]awg.Peer, 0, len(c.Peers))
	for _, p := range c.Peers {
		peers = append(peers, awg.Peer{PublicKey: p.PublicKey, AllowedIPs: p.AllowedIPs})
	}
	return awg.ServerConfig{
		PrivateKey: c.PrivateKey,
		ListenPort: c.ListenPort,
		Params:     c.Params,
		Peers:      peers,
	}
}

// LoadConfig reads and validates a configuration file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Marshal renders the configuration for writing to a server.
func (c *Config) Marshal() ([]byte, error) {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// Save writes the configuration with owner-only permissions: it holds the
// server's private key.
func (c *Config) Save(path string) error {
	raw, err := c.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
