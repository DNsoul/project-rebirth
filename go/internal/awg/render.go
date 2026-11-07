package awg

import (
	"fmt"
	"strconv"
	"strings"
)

// Peer is one end the interface talks to.
//
// On a server it is a client that is accepted, and needs only its key and the
// addresses it may use. On a client it is the server, and also needs where to
// reach it.
type Peer struct {
	PublicKey  Key
	AllowedIPs []string

	// Endpoint is "host:port". Servers leave it empty: they learn a client's
	// address from its first packet.
	Endpoint string

	// Keepalive is the interval in seconds between keepalive packets, or zero
	// for none.
	Keepalive int
}

// ServerConfig is everything the node needs to bring its interface up.
type ServerConfig struct {
	PrivateKey Key
	ListenPort int
	Params     Params
	Peers      []Peer
}

// UAPI renders the configuration in the text form amneziawg-go reads, as
// described at wireguard.com/xplatform. Keys are hex here; the .conf form
// uses base64.
//
// H1-H4 are left out so the device keeps its defaults, which is what the
// documentation asks for when a header protection key is set.
func (c ServerConfig) UAPI() string {
	var b strings.Builder

	line := func(k, v string) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	num := func(k string, v int) { line(k, strconv.Itoa(v)) }

	line("private_key", c.PrivateKey.Hex())
	num("listen_port", c.ListenPort)

	p := c.Params
	num("jc", p.Jc)
	num("jmin", p.Jmin)
	num("jmax", p.Jmax)
	num("s1", p.S1)
	num("s2", p.S2)
	num("s3", p.S3)
	num("s4", p.S4)
	if !p.HeaderProtectionKey.IsZero() {
		line("header_protection_key", p.HeaderProtectionKey.Hex())
	}
	// amneziawg-go parses this with strconv.ParseBool, which rejects
	// "on"/"off".
	line("random_trailers", strconv.FormatBool(p.RandomTrailers))
	if p.I1 != "" {
		line("i1", p.I1)
	}

	// Replace the peer set wholesale so reconfiguring never leaves a
	// stale client behind.
	line("replace_peers", "true")
	for _, peer := range c.Peers {
		line("public_key", peer.PublicKey.Hex())
		if peer.Endpoint != "" {
			line("endpoint", peer.Endpoint)
		}
		if peer.Keepalive > 0 {
			num("persistent_keepalive_interval", peer.Keepalive)
		}
		line("replace_allowed_ips", "true")
		for _, cidr := range peer.AllowedIPs {
			line("allowed_ip", cidr)
		}
	}
	return b.String()
}

// BOM is the UTF-8 byte order mark, written as explicit bytes so it cannot
// sit invisibly in this source file.
//
// It matters because a shell redirection on Windows happily prepends one, and
// the AmneziaWG client then rejects the entire configuration with "Line must
// occur in a section" — a message that points at the syntax rather than at
// the encoding.
const BOM = "\xef\xbb\xbf"

// ClientConfig is one client's view of the tunnel.
type ClientConfig struct {
	PrivateKey Key
	Address    string
	DNS        []string
	MTU        int
	Params     Params

	ServerPublicKey Key
	Endpoint        string
	AllowedIPs      []string
	Keepalive       int
}

// Render writes the wg-quick style file that both our own apps and the stock
// AmneziaWG clients read.
//
// Booleans are written as 1 and 0: amneziawg-go rejects "on"/"off" while
// amneziawg-tools accepts either, so digits are the form every end
// understands.
func (c ClientConfig) Render() string {
	var b strings.Builder

	kv := func(k, v string) {
		if v == "" {
			return
		}
		fmt.Fprintf(&b, "%s = %s\n", k, v)
	}
	num := func(k string, v int) { kv(k, strconv.Itoa(v)) }

	mtu := c.MTU
	if mtu == 0 {
		mtu = MTU
	}

	b.WriteString("[Interface]\n")
	kv("PrivateKey", c.PrivateKey.Base64())
	kv("Address", c.Address)
	if len(c.DNS) > 0 {
		kv("DNS", strings.Join(c.DNS, ", "))
	}
	num("MTU", mtu)

	p := c.Params
	num("Jc", p.Jc)
	num("Jmin", p.Jmin)
	num("Jmax", p.Jmax)
	num("S1", p.S1)
	num("S2", p.S2)
	num("S3", p.S3)
	num("S4", p.S4)
	if !p.HeaderProtectionKey.IsZero() {
		kv("HeaderProtectionKey", p.HeaderProtectionKey.Base64())
	}
	kv("RandomTrailers", boolDigit(p.RandomTrailers))
	kv("I1", p.I1)

	keepalive := c.Keepalive
	if keepalive == 0 {
		keepalive = Keepalive
	}

	b.WriteString("\n[Peer]\n")
	kv("PublicKey", c.ServerPublicKey.Base64())
	kv("AllowedIPs", strings.Join(c.AllowedIPs, ", "))
	kv("Endpoint", c.Endpoint)
	num("PersistentKeepalive", keepalive)

	return b.String()
}

func boolDigit(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
