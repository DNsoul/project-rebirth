package splitdns

import (
	"net/netip"
	"strings"
	"testing"

	"awgrotator/internal/ruset"
)

func ip(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("bad test address %q: %v", s, err)
	}
	return a
}

func ips(t *testing.T, ss ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = ip(t, s)
	}
	return out
}

func mustRules(t *testing.T, names ...string) *Rules {
	t.Helper()
	r, err := NewRules(names...)
	if err != nil {
		t.Fatalf("NewRules: %v", err)
	}
	return r
}

// localSet is a stand-in for the Russian ranges: 5.0.0.0/8 and 77.88.0.0/16.
func localSet(t *testing.T) *ruset.Set {
	t.Helper()
	var b ruset.Builder
	for _, p := range []string{"5.0.0.0/8", "77.88.0.0/16"} {
		if err := b.AddPrefix(netip.MustParsePrefix(p)); err != nil {
			t.Fatal(err)
		}
	}
	return b.Build()
}

func TestRulesMatchSuffixes(t *testing.T) {
	r := mustRules(t, "mc.yandex.ru", "example.com")

	for name, want := range map[string]bool{
		"mc.yandex.ru":       true,
		"a.b.mc.yandex.ru":   true,
		"MC.Yandex.RU":       true,  // case does not matter
		"mc.yandex.ru.":      true,  // trailing dot of a fully qualified name
		"yandex.ru":          false, // the parent is not covered by a child rule
		"xmc.yandex.ru":      false, // a label boundary, not a substring
		"notexample.com":     false,
		"example.com":        true,
		"www.example.com":    true,
		"example.com.evil.x": false,
		"":                   false,
	} {
		if got := r.Match(name); got != want {
			t.Errorf("Match(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNilAndEmptyRulesMatchNothing(t *testing.T) {
	var nilRules *Rules
	if nilRules.Match("example.com") || nilRules.Len() != 0 {
		t.Error("a nil rule set matched something")
	}
	if mustRules(t).Match("example.com") {
		t.Error("an empty rule set matched something")
	}
}

func TestParseRules(t *testing.T) {
	input := `# trackers
mc.yandex.ru
  *.counter.yadro.ru   # wildcard syntax
EXAMPLE.org.

сайт.рф
`
	r, err := ParseRules(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseRules: %v", err)
	}
	if r.Len() != 4 {
		t.Errorf("Len() = %d, want 4", r.Len())
	}
	for _, name := range []string{
		"mc.yandex.ru", "x.counter.yadro.ru", "counter.yadro.ru", "example.org",
		// An internationalised name is matched in its wire (punycode) form.
		"xn--80aswg.xn--p1ai",
	} {
		if !r.Match(name) {
			t.Errorf("%q was not matched", name)
		}
	}
}

func TestParseRulesRejectsNonsenseLoudly(t *testing.T) {
	for name, line := range map[string]string{
		"a url":         "https://example.com/path",
		"has a space":   "two words",
		"empty label":   "a..b",
		"a port":        "example.com:8080",
		"label too big": strings.Repeat("a", 64) + ".com",
	} {
		_, err := ParseRules(strings.NewReader(line + "\n"))
		if err == nil {
			t.Errorf("%s: %q was accepted", name, line)
			continue
		}
		if !strings.Contains(err.Error(), "line 1") && name != "a url" {
			// Every error should say where the bad line is.
			t.Errorf("%s: error does not name the line: %v", name, err)
		}
	}
}

func TestDecide(t *testing.T) {
	d := &Decider{
		RU:          localSet(t),
		ForceTunnel: mustRules(t, "tracker.example"),
		ForceDirect: mustRules(t, "bank.example"),
	}

	cases := []struct {
		name  string
		host  string
		addrs []string
		want  Verdict
	}{
		{"all local", "site.ru", []string{"5.1.1.1", "77.88.1.1"}, Direct},
		{"all foreign", "site.com", []string{"142.250.1.1"}, Tunnel},
		// The rule that protects the home address: one foreign record sends
		// the whole name through the tunnel.
		{"mixed", "site.ru", []string{"5.1.1.1", "142.250.1.1"}, Tunnel},
		{"nothing resolved", "site.ru", nil, Tunnel},
		{"forced tunnel beats local addresses", "tracker.example", []string{"5.1.1.1"}, Tunnel},
		{"forced direct beats foreign addresses", "bank.example", []string{"142.250.1.1"}, Direct},
		{"forced rule covers subdomains", "www.tracker.example", []string{"5.1.1.1"}, Tunnel},
	}
	for _, tc := range cases {
		got := d.Decide(tc.host, ips(t, tc.addrs...))
		if got.Verdict != tc.want {
			t.Errorf("%s: %s (%s), want %s", tc.name, got.Verdict, got.Reason, tc.want)
		}
		if got.Reason == "" {
			t.Errorf("%s: no reason given", tc.name)
		}
	}
}

func TestForceTunnelWinsOverForceDirect(t *testing.T) {
	d := &Decider{
		RU:          localSet(t),
		ForceTunnel: mustRules(t, "both.example"),
		ForceDirect: mustRules(t, "both.example"),
	}
	if got := d.Decide("both.example", ips(t, "5.1.1.1")); got.Verdict != Tunnel {
		t.Errorf("a name in both lists went %s, want the safe direction (tunnel)", got.Verdict)
	}
}

func TestDeciderWithEmptyRUSetSendsEverythingThroughTheTunnel(t *testing.T) {
	// If the set failed to load, nothing looks local. That must fail towards
	// the tunnel, not towards the home connection.
	d := &Decider{}
	if got := d.Decide("site.ru", ips(t, "5.1.1.1")); got.Verdict != Tunnel {
		t.Errorf("with no local set, a site went %s", got.Verdict)
	}
}

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":         true,
		"142.250.1.1":     true,
		"77.88.8.8":       true,
		"172.15.255.255":  true, // just outside 172.16/12
		"172.32.0.0":      true, // just outside it on the other side
		"100.63.255.255":  true, // just outside CGNAT
		"100.128.0.0":     true,
		"169.253.255.255": true,

		"0.0.0.0":         false,
		"10.8.0.1":        false, // the tunnel's own subnet
		"10.255.255.255":  false,
		"127.0.0.1":       false,
		"169.254.169.254": false, // cloud metadata
		"172.16.0.1":      false,
		"172.31.255.255":  false,
		"192.168.1.1":     false,
		"100.64.0.1":      false,
		"198.18.0.1":      false, // the stand-in range
		"198.19.255.254":  false,
		"224.0.0.1":       false,
		"255.255.255.255": false,
		"192.0.2.1":       false,
		"2001:db8::1":     false, // IPv6 is not handled here at all
	} {
		if got := IsPublic(ip(t, addr)); got != want {
			t.Errorf("IsPublic(%s) = %v, want %v", addr, got, want)
		}
	}
	if !IsPublic(ip(t, "::ffff:8.8.8.8")) {
		t.Error("an IPv4-mapped public address was not recognised")
	}
	if IsPublic(ip(t, "::ffff:127.0.0.1")) {
		t.Error("an IPv4-mapped loopback address was treated as public")
	}
}

func TestFilterPublic(t *testing.T) {
	got := FilterPublic(ips(t, "8.8.8.8", "127.0.0.1", "169.254.169.254", "77.88.8.8", "0.0.0.0"))
	if len(got) != 2 || got[0].String() != "8.8.8.8" || got[1].String() != "77.88.8.8" {
		t.Errorf("FilterPublic = %v, want [8.8.8.8 77.88.8.8]", got)
	}
	if got := FilterPublic(nil); len(got) != 0 {
		t.Errorf("FilterPublic(nil) = %v", got)
	}
}
