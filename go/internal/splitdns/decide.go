package splitdns

import (
	"net/netip"

	"awgrotator/internal/ruset"
)

// Verdict is where a name's traffic goes.
type Verdict int

const (
	// Direct means the client gets the real addresses and connects over its
	// own connection. The site sees the home address.
	Direct Verdict = iota + 1

	// Tunnel means the client gets stand-in addresses and the connection goes
	// through the server. The site sees the server's address.
	Tunnel
)

func (v Verdict) String() string {
	switch v {
	case Direct:
		return "direct"
	case Tunnel:
		return "tunnel"
	}
	return "unknown"
}

// Decision is a verdict with the reason for it, kept so the log can say why a
// site went the way it did.
type Decision struct {
	Verdict Verdict
	Reason  string
}

// Decider applies the routing policy.
type Decider struct {
	// RU is the set of addresses that count as local.
	RU *ruset.Set

	// ForceTunnel and ForceDirect override the address-based decision for
	// named sites. When a name is in both, tunnel wins: the safe direction is
	// the one that hides the home address.
	ForceTunnel *Rules
	ForceDirect *Rules
}

// Decide chooses a verdict for a name given every address it resolved to.
//
// A name goes direct only when all of its addresses are local. One foreign
// address is enough to send it through the tunnel: a site that is partly
// hosted abroad would otherwise reveal the home address to the part that is.
//
// An empty address list is a tunnel verdict. There is nothing to connect to
// either way, but failing towards the tunnel is the safe default.
func (d *Decider) Decide(name string, addrs []netip.Addr) Decision {
	switch {
	case d.ForceTunnel.Match(name):
		return Decision{Tunnel, "forced into the tunnel"}
	case d.ForceDirect.Match(name):
		return Decision{Direct, "forced direct"}
	}
	if len(addrs) == 0 {
		return Decision{Tunnel, "no addresses"}
	}
	for _, a := range addrs {
		if !d.RU.Contains(a) {
			return Decision{Tunnel, "foreign address " + a.String()}
		}
	}
	return Decision{Direct, "all addresses are local"}
}

// special lists the IPv4 ranges that are never a legitimate public
// destination. They matter for security, not tidiness.
//
// Stand-in addresses are translated to whatever real address a DNS answer
// contained. Without this filter, a hostile domain could answer with
// 169.254.169.254 or 127.0.0.1 or an address in the tunnel's own subnet, and a
// client that resolved it would be connected to the server's metadata service
// or to the server itself.
var special = mustPrefixes(
	"0.0.0.0/8",       // "this network", and the 0.0.0.0 sinkhole answer
	"10.0.0.0/8",      // private
	"100.64.0.0/10",   // carrier-grade NAT
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link local, including cloud metadata services
	"172.16.0.0/12",   // private
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"192.168.0.0/16",  // private
	"198.18.0.0/15",   // the stand-in range itself, and benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, and the broadcast address
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}

// IsPublic reports whether an address may be a real destination.
func IsPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() {
		return false
	}
	for _, p := range special {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// FilterPublic returns the public addresses, preserving order.
func FilterPublic(addrs []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if IsPublic(a) {
			out = append(out, a.Unmap())
		}
	}
	return out
}
