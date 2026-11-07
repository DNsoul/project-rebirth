package splitdns

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"time"
)

// NFT keeps the kernel's stand-in translations in an nftables map and owns the
// two rules that use it. It touches only its own table, so nothing installed
// by anything else is disturbed.
//
// Two rules do the work:
//
//	translate: a packet arriving from the tunnel addressed to the stand-in
//	           range has its destination replaced by the real address found in
//	           the map. This runs before routing, so the packet is then routed
//	           and masqueraded like any other.
//
// Both rules carry counters, so that when a client reports that a site does
// not load it is possible to see whether its packets reached the translation
// and whether any were refused for lacking one.
//
//	guard:     a packet still addressed to the stand-in range after that has
//	           no translation. It is rejected at once. Without this it would be
//	           forwarded to the internet, where the range is not routable, and
//	           the client would wait out a connection timeout instead of being
//	           told promptly.
type NFT struct {
	// Table and Map name what is created. Defaults are "awgsplit" and
	// "fake2real".
	Table string
	Map   string

	// Interface is the tunnel device that traffic must arrive on.
	Interface string

	// Range is the stand-in range and DNS the resolver's own address inside
	// it, which is local traffic and is left alone.
	Range netip.Prefix
	DNS   netip.Addr

	// Run executes an nft script. Tests replace it; the default pipes the
	// script to "nft -f -".
	Run func(ctx context.Context, script string) (string, error)
}

// chunk is how many elements go into one statement of a bulk load.
const chunk = 500

func (n *NFT) table() string   { return orDefault(n.Table, "awgsplit") }
func (n *NFT) mapName() string { return orDefault(n.Map, "fake2real") }

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// Ruleset renders the table, its map and its two chains. It is a pure
// function so the exact text can be tested.
//
// Declaring the table, deleting it and declaring it again makes the script
// idempotent: the first line creates the table when it is missing so that the
// delete always has something to remove.
func (n *NFT) Ruleset() string {
	return fmt.Sprintf(`table ip %[1]s {}
delete table ip %[1]s
table ip %[1]s {
	map %[2]s {
		type ipv4_addr : ipv4_addr
	}
	chain translate {
		type nat hook prerouting priority dstnat; policy accept;
		iifname %[3]q ip daddr %[4]s ip daddr != %[5]s counter dnat to ip daddr map @%[2]s
	}
	chain guard {
		type filter hook forward priority -10; policy accept;
		iifname %[3]q ip daddr %[4]s counter reject with icmp type host-unreachable
	}
}
`, n.table(), n.mapName(), n.Interface, n.Range, n.DNS)
}

// Setup creates the table, replacing any earlier one. Everything in the map is
// lost by this, so a caller restoring saved translations does that afterwards.
func (n *NFT) Setup(ctx context.Context) error {
	if !n.Range.IsValid() || !n.DNS.IsValid() || n.Interface == "" {
		return fmt.Errorf("nft: range, resolver address and interface are all required")
	}
	if out, err := n.run(ctx, n.Ruleset()); err != nil {
		return fmt.Errorf("create %s: %w: %s", n.table(), err, out)
	}
	return nil
}

// Add installs one translation.
func (n *NFT) Add(fake, real netip.Addr) error {
	script := fmt.Sprintf("add element ip %s %s { %s : %s }\n", n.table(), n.mapName(), fake, real)
	return n.runTimed(script)
}

// Delete removes one translation. Removing one that is already gone is not an
// error: the goal is that it does not exist.
func (n *NFT) Delete(fake netip.Addr) error {
	script := fmt.Sprintf("delete element ip %s %s { %s }\n", n.table(), n.mapName(), fake)
	err := n.runTimed(script)
	if err != nil && strings.Contains(err.Error(), "No such file or directory") {
		return nil
	}
	return err
}

// Replace makes the map hold exactly these translations, in one transaction,
// so there is no moment when it is half loaded.
func (n *NFT) Replace(entries []Entry) error {
	var b strings.Builder
	fmt.Fprintf(&b, "flush map ip %s %s\n", n.table(), n.mapName())
	for start := 0; start < len(entries); start += chunk {
		end := min(start+chunk, len(entries))
		fmt.Fprintf(&b, "add element ip %s %s { ", n.table(), n.mapName())
		for i, e := range entries[start:end] {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s : %s", e.Fake, e.Real)
		}
		b.WriteString(" }\n")
	}
	return n.runTimed(b.String())
}

func (n *NFT) runTimed(script string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := n.run(ctx, script); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (n *NFT) run(ctx context.Context, script string) (string, error) {
	if n.Run != nil {
		return n.Run(ctx, script)
	}
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
