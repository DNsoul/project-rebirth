package splitdns

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

type scriptRecorder struct {
	scripts []string
	err     error
	out     string
}

func (r *scriptRecorder) run(_ context.Context, script string) (string, error) {
	r.scripts = append(r.scripts, script)
	return r.out, r.err
}

func testNFT(rec *scriptRecorder) *NFT {
	return &NFT{
		Interface: "awg0",
		Range:     netip.MustParsePrefix("198.18.0.0/15"),
		DNS:       netip.MustParseAddr("198.18.0.1"),
		Run:       rec.run,
	}
}

func TestRulesetText(t *testing.T) {
	got := testNFT(&scriptRecorder{}).Ruleset()

	for _, want := range []string{
		// Idempotent: create if missing, delete, recreate.
		"table ip awgsplit {}\ndelete table ip awgsplit\ntable ip awgsplit {",
		"map fake2real {\n\t\ttype ipv4_addr : ipv4_addr",
		// Translation happens before routing, only for traffic from the
		// tunnel, and never for the resolver's own address.
		"type nat hook prerouting priority dstnat",
		`iifname "awg0" ip daddr 198.18.0.0/15 ip daddr != 198.18.0.1 counter dnat to ip daddr map @fake2real`,
		// Untranslated traffic is refused promptly, and before the host
		// firewall gets a say.
		"type filter hook forward priority -10",
		`iifname "awg0" ip daddr 198.18.0.0/15 counter reject with icmp type host-unreachable`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ruleset is missing %q:\n%s", want, got)
		}
	}
}

func TestRulesetUsesCustomNames(t *testing.T) {
	n := testNFT(&scriptRecorder{})
	n.Table, n.Map = "other", "m"
	got := n.Ruleset()
	if !strings.Contains(got, "table ip other {}") || !strings.Contains(got, "@m") {
		t.Errorf("custom names were not used:\n%s", got)
	}
	if strings.Contains(got, "awgsplit") || strings.Contains(got, "fake2real") {
		t.Errorf("default names leaked into a custom ruleset:\n%s", got)
	}
}

func TestSetupRunsTheRuleset(t *testing.T) {
	rec := &scriptRecorder{}
	n := testNFT(rec)
	if err := n.Setup(context.Background()); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if len(rec.scripts) != 1 || rec.scripts[0] != n.Ruleset() {
		t.Errorf("Setup ran %d scripts", len(rec.scripts))
	}
}

func TestSetupNeedsItsParameters(t *testing.T) {
	for name, n := range map[string]*NFT{
		"no range":     {Interface: "awg0", DNS: netip.MustParseAddr("198.18.0.1")},
		"no resolver":  {Interface: "awg0", Range: netip.MustParsePrefix("198.18.0.0/15")},
		"no interface": {Range: netip.MustParsePrefix("198.18.0.0/15"), DNS: netip.MustParseAddr("198.18.0.1")},
	} {
		if err := n.Setup(context.Background()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSetupSurfacesNFTOutput(t *testing.T) {
	rec := &scriptRecorder{err: errors.New("exit status 1"), out: "Error: syntax error near here"}
	err := testNFT(rec).Setup(context.Background())
	if err == nil {
		t.Fatal("a failing nft was not reported")
	}
	if !strings.Contains(err.Error(), "syntax error near here") {
		t.Errorf("the error hides nft's own explanation: %v", err)
	}
}

func TestAddAndDeleteScripts(t *testing.T) {
	rec := &scriptRecorder{}
	n := testNFT(rec)

	if err := n.Add(netip.MustParseAddr("198.18.0.5"), netip.MustParseAddr("142.250.1.1")); err != nil {
		t.Fatal(err)
	}
	if err := n.Delete(netip.MustParseAddr("198.18.0.5")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"add element ip awgsplit fake2real { 198.18.0.5 : 142.250.1.1 }\n",
		"delete element ip awgsplit fake2real { 198.18.0.5 }\n",
	}
	if len(rec.scripts) != 2 || rec.scripts[0] != want[0] || rec.scripts[1] != want[1] {
		t.Errorf("scripts = %q, want %q", rec.scripts, want)
	}
}

func TestDeletingAMissingElementIsNotAnError(t *testing.T) {
	rec := &scriptRecorder{
		err: errors.New("exit status 1"),
		out: "Error: Could not process rule: No such file or directory",
	}
	if err := testNFT(rec).Delete(netip.MustParseAddr("198.18.0.5")); err != nil {
		t.Errorf("deleting something already gone failed: %v", err)
	}

	// Any other failure must still surface.
	rec2 := &scriptRecorder{err: errors.New("exit status 1"), out: "Error: Operation not permitted"}
	if err := testNFT(rec2).Delete(netip.MustParseAddr("198.18.0.5")); err == nil {
		t.Error("an unrelated delete failure was swallowed")
	}
}

func TestReplaceFlushesThenLoadsInChunks(t *testing.T) {
	rec := &scriptRecorder{}
	n := testNFT(rec)

	var entries []Entry
	for i := 0; i < chunk*2+10; i++ {
		entries = append(entries, Entry{
			Fake: netip.AddrFrom4([4]byte{198, 18, byte(i / 250), byte(i%250 + 2)}),
			Real: netip.AddrFrom4([4]byte{8, 8, byte(i / 250), byte(i%250 + 1)}),
		})
	}
	if err := n.Replace(entries); err != nil {
		t.Fatal(err)
	}

	// One script, so the whole load is a single transaction.
	if len(rec.scripts) != 1 {
		t.Fatalf("Replace ran %d scripts, want 1 (atomic)", len(rec.scripts))
	}
	lines := strings.Split(strings.TrimSpace(rec.scripts[0]), "\n")
	if lines[0] != "flush map ip awgsplit fake2real" {
		t.Errorf("first line = %q, want the flush", lines[0])
	}
	if len(lines) != 1+3 {
		t.Errorf("%d lines, want a flush and 3 chunked statements", len(lines))
	}
	if got := strings.Count(rec.scripts[0], " : "); got != len(entries) {
		t.Errorf("%d elements in the script, want %d", got, len(entries))
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "add element ip awgsplit fake2real { ") || !strings.HasSuffix(l, " }") {
			t.Errorf("malformed statement: %.80s", l)
		}
	}
}

func TestReplaceWithNothingJustFlushes(t *testing.T) {
	rec := &scriptRecorder{}
	if err := testNFT(rec).Replace(nil); err != nil {
		t.Fatal(err)
	}
	if rec.scripts[0] != "flush map ip awgsplit fake2real\n" {
		t.Errorf("script = %q", rec.scripts[0])
	}
}

// Every value in these scripts is a netip.Addr, never a string from outside,
// so nothing a name or an answer contains can end up as nft syntax. Pin that
// down: the types make it true, this test makes it stay true.
func TestScriptsContainOnlyAddressesAndFixedText(t *testing.T) {
	rec := &scriptRecorder{}
	n := testNFT(rec)
	_ = n.Add(netip.MustParseAddr("198.18.0.5"), netip.MustParseAddr("142.250.1.1"))
	_ = n.Replace([]Entry{{Fake: netip.MustParseAddr("198.18.0.6"), Real: netip.MustParseAddr("1.1.1.1")}})

	for _, s := range rec.scripts {
		for _, bad := range []string{";", "`", "$", "\\"} {
			if strings.Contains(s, bad) {
				t.Errorf("script contains %q:\n%s", bad, s)
			}
		}
	}
}
