package usage

import "net/netip"

// Classifier tells which site an address beyond the server is.
type Classifier struct {
	// StandIns is the range the resolver hands stand-in addresses from, and
	// Names the site each one was handed out for, "" when not known.
	StandIns netip.Prefix
	Names    func(fake netip.Addr) string

	// Server are the server's own addresses: the tunnel network and the
	// resolver.
	Server []netip.Prefix

	// Telegram are the networks the clients route through the tunnel by
	// address.
	Telegram []netip.Prefix
}

// Site is what the address belongs to.
func (c Classifier) Site(remote netip.Addr) string {
	if c.StandIns.IsValid() && c.StandIns.Contains(remote) {
		if c.Names != nil {
			if s := c.Names(remote); s != "" {
				return s
			}
		}
		return SiteUnknown
	}
	for _, p := range c.Server {
		if p.Contains(remote) {
			return SiteServer
		}
	}
	for _, p := range c.Telegram {
		if p.Contains(remote) {
			return SiteTelegram
		}
	}
	return SiteElsewhere
}

// Records turns counted flows into traffic of devices to sites. device tells
// which device has a tunnel address; flows of addresses that belong to no
// device (the shared registration peer) are left out.
func Records(flows map[Flow]Bytes, device func(netip.Addr) (string, bool), c Classifier) []Record {
	sums := map[[2]string]Bytes{}
	for f, b := range flows {
		id, ok := device(f.Client)
		if !ok {
			continue
		}
		k := [2]string{id, c.Site(f.Remote)}
		v := sums[k]
		v.add(b)
		sums[k] = v
	}
	out := make([]Record, 0, len(sums))
	for k, b := range sums {
		out = append(out, Record{Device: k[0], Site: k[1], Bytes: b})
	}
	return out
}
