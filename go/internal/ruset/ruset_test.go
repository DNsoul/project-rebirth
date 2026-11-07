package ruset

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
)

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("bad test address %q: %v", s, err)
	}
	return a
}

func prefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatalf("bad test prefix %q: %v", s, err)
	}
	return p
}

func TestContainsBoundaries(t *testing.T) {
	var b Builder
	if err := b.AddPrefix(prefix(t, "10.0.0.0/24")); err != nil {
		t.Fatal(err)
	}
	if err := b.AddPrefix(prefix(t, "192.168.0.0/16")); err != nil {
		t.Fatal(err)
	}
	s := b.Build()

	cases := []struct {
		ip   string
		want bool
	}{
		{"9.255.255.255", false}, // one before the first range
		{"10.0.0.0", true},       // first address
		{"10.0.0.255", true},     // last address
		{"10.0.1.0", false},      // one past
		{"192.167.255.255", false},
		{"192.168.0.0", true},
		{"192.168.255.255", true},
		{"192.169.0.0", false},
		{"0.0.0.0", false},
		{"255.255.255.255", false},
	}
	for _, tc := range cases {
		if got := s.Contains(addr(t, tc.ip)); got != tc.want {
			t.Errorf("Contains(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestEmptyAndNilSetsContainNothing(t *testing.T) {
	var nilSet *Set
	if nilSet.Contains(addr(t, "10.0.0.1")) {
		t.Error("a nil set contains an address")
	}
	if got := (&Builder{}).Build(); got.Contains(addr(t, "10.0.0.1")) || got.Ranges() != 0 {
		t.Error("an empty set is not empty")
	}
	if nilSet.Ranges() != 0 || nilSet.Addresses() != 0 {
		t.Error("a nil set reports a size")
	}
}

func TestBuildMergesOverlapAndAdjacency(t *testing.T) {
	var b Builder
	for _, p := range []string{
		"10.0.0.0/25",   // 10.0.0.0   - 10.0.0.127
		"10.0.0.128/25", // 10.0.0.128 - 10.0.0.255, touches the one above
		"10.0.0.64/26",  // wholly inside the first
		"10.0.1.0/24",   // touches the merged block
		"10.0.3.0/24",   // gap before it, stays separate
	} {
		if err := b.AddPrefix(prefix(t, p)); err != nil {
			t.Fatal(err)
		}
	}
	s := b.Build()

	if got := s.Ranges(); got != 2 {
		t.Errorf("Ranges() = %d, want 2 (10.0.0.0-10.0.1.255 and 10.0.3.0-10.0.3.255)", got)
	}
	if got := s.Addresses(); got != 512+256 {
		t.Errorf("Addresses() = %d, want %d", got, 512+256)
	}
	if s.Contains(addr(t, "10.0.2.0")) {
		t.Error("the gap between the two ranges was filled in")
	}
}

// RIR delegations are not CIDR blocks: the count need not be a power of two
// and the start need not be aligned to it.
func TestAddCountHandlesUnalignedRanges(t *testing.T) {
	var b Builder
	// 3 addresses starting at an odd offset.
	if err := b.AddCount(0x0A000001, 3); err != nil {
		t.Fatal(err)
	}
	s := b.Build()

	for ip, want := range map[string]bool{
		"10.0.0.0": false,
		"10.0.0.1": true,
		"10.0.0.2": true,
		"10.0.0.3": true,
		"10.0.0.4": false,
	} {
		if got := s.Contains(addr(t, ip)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestRangeAtTheTopOfTheAddressSpace(t *testing.T) {
	var b Builder
	if err := b.AddPrefix(prefix(t, "255.255.255.0/24")); err != nil {
		t.Fatal(err)
	}
	// A touching range after this one would need end+1, which wraps.
	if err := b.AddPrefix(prefix(t, "255.255.254.0/24")); err != nil {
		t.Fatal(err)
	}
	s := b.Build()

	if s.Ranges() != 1 {
		t.Errorf("Ranges() = %d, want 1", s.Ranges())
	}
	if !s.Contains(addr(t, "255.255.255.255")) {
		t.Error("the last address in the space was lost")
	}
	if got := s.Addresses(); got != 512 {
		t.Errorf("Addresses() = %d, want 512", got)
	}
}

func TestAddCountRejectsBadInput(t *testing.T) {
	var b Builder
	if err := b.AddCount(0x0A000000, 0); err == nil {
		t.Error("an empty range was accepted")
	}
	if err := b.AddCount(0xFFFFFF00, 512); err == nil {
		t.Error("a range running past the end of the address space was accepted")
	}
	if err := b.AddRange(20, 10); err == nil {
		t.Error("a backwards range was accepted")
	}
	if err := b.AddPrefix(prefix(t, "2001:db8::/32")); err == nil {
		t.Error("an IPv6 prefix was accepted")
	}
}

func TestContainsRefusesIPv6AndUnmapsMapped(t *testing.T) {
	var b Builder
	if err := b.AddPrefix(prefix(t, "10.0.0.0/8")); err != nil {
		t.Fatal(err)
	}
	s := b.Build()

	if s.Contains(addr(t, "2001:db8::1")) {
		t.Error("an IPv6 address matched an IPv4 set")
	}
	// An IPv4-mapped IPv6 address is the same host and must be treated so.
	if !s.Contains(addr(t, "::ffff:10.1.2.3")) {
		t.Error("an IPv4-mapped address was not recognised")
	}
}

const sampleRIR = `2|ripencc|20261106|165432|19830705|20261106|+0200
ripencc|*|ipv4|*|35000|summary
ripencc|RU|ipv4|2.60.0.0|524288|20120508|allocated|abc
ripencc|RU|ipv4|5.8.0.0|65536|20120601|assigned|def
ripencc|RU|ipv4|9.9.9.0|256|20120601|available|ghi
ripencc|RU|ipv4|9.9.10.0|256|20120601|reserved|jkl
ripencc|DE|ipv4|5.9.0.0|65536|20120601|allocated|mno
ripencc|RU|ipv6|2a00:1000::|32|20120601|allocated|pqr
ripencc|ru|ipv4|100.64.0.0|1000|20120601|allocated|lowercase country, odd count
# a comment line
ripencc|RU|asn|1234|1|20120601|allocated|not an address
`

func TestParseRIRKeepsOnlyDelegatedIPv4ForTheCountry(t *testing.T) {
	s, err := ParseRIR(strings.NewReader(sampleRIR), "RU")
	if err != nil {
		t.Fatalf("ParseRIR: %v", err)
	}

	for ip, want := range map[string]bool{
		"2.60.0.0":     true,  // allocated
		"2.67.255.255": true,  // last of 524288 addresses
		"2.68.0.0":     false, // one past
		"5.8.200.1":    true,  // assigned
		"5.9.0.1":      false, // delegated to DE
		"9.9.9.1":      false, // available, nobody uses it
		"9.9.10.1":     false, // reserved
		"100.64.3.231": true,  // odd count of 1000 addresses
		"100.64.3.232": false, // one past 1000
	} {
		if got := s.Contains(addr(t, ip)); got != want {
			t.Errorf("Contains(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestParseRIRErrors(t *testing.T) {
	if _, err := ParseRIR(strings.NewReader(sampleRIR), "R"); err == nil {
		t.Error("a one-letter country was accepted")
	}
	if _, err := ParseRIR(strings.NewReader(sampleRIR), "ZZ"); err == nil {
		t.Error("a country with no ranges was not reported as empty")
	}
	if _, err := ParseRIR(strings.NewReader(""), "RU"); err == nil {
		t.Error("an empty file was accepted")
	}
	bad := "ripencc|RU|ipv4|not-an-address|256|20120601|allocated|x\n"
	if _, err := ParseRIR(strings.NewReader(bad), "RU"); err == nil {
		t.Error("a bad address was accepted")
	}
	bad = "ripencc|RU|ipv4|10.0.0.0|many|20120601|allocated|x\n"
	if _, err := ParseRIR(strings.NewReader(bad), "RU"); err == nil {
		t.Error("a bad count was accepted")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	s, err := ParseRIR(strings.NewReader(sampleRIR), "RU")
	if err != nil {
		t.Fatalf("ParseRIR: %v", err)
	}

	var buf bytes.Buffer
	if _, err := s.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	back, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if back.Ranges() != s.Ranges() || back.Addresses() != s.Addresses() {
		t.Errorf("round trip changed the set: %d ranges / %d addresses, was %d / %d",
			back.Ranges(), back.Addresses(), s.Ranges(), s.Addresses())
	}
	for _, ip := range []string{"2.60.0.0", "2.67.255.255", "5.8.0.1", "100.64.3.231"} {
		if !back.Contains(addr(t, ip)) {
			t.Errorf("%s was lost in the round trip", ip)
		}
	}
	if back.Contains(addr(t, "5.9.0.1")) {
		t.Error("a foreign address appeared after the round trip")
	}
}

func TestReadRejectsMalformedLines(t *testing.T) {
	for name, in := range map[string]string{
		"no dash":   "10.0.0.0\n",
		"bad start": "nope-10.0.0.9\n",
		"bad end":   "10.0.0.0-nope\n",
		"backwards": "10.0.0.9-10.0.0.0\n",
		"ipv6":      "2001:db8::-2001:db8::ff\n",
	} {
		if _, err := Read(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	ok := "# comment\n\n10.0.0.0-10.0.0.255\n"
	s, err := Read(strings.NewReader(ok))
	if err != nil {
		t.Fatalf("a valid file with comments was rejected: %v", err)
	}
	if !s.Contains(addr(t, "10.0.0.7")) {
		t.Error("the range was not read")
	}
}

// The lookup is binary search over merged ranges; check it against the
// obvious linear scan so a mistake in the search cannot hide.
func TestContainsAgreesWithLinearScan(t *testing.T) {
	var b Builder
	type span struct{ lo, hi uint32 }
	spans := []span{
		{100, 199}, {300, 300}, {500, 899}, {901, 950}, {2000, 2999},
		{4000, 4000}, {5000, 5100}, {1 << 31, 1<<31 + 5000},
	}
	for _, sp := range spans {
		if err := b.AddRange(sp.lo, sp.hi); err != nil {
			t.Fatal(err)
		}
	}
	s := b.Build()

	linear := func(v uint32) bool {
		for _, sp := range spans {
			if v >= sp.lo && v <= sp.hi {
				return true
			}
		}
		return false
	}
	for v := uint32(0); v < 6000; v++ {
		if got, want := s.Contains(fromUint32(v)), linear(v); got != want {
			t.Fatalf("Contains(%d) = %v, linear scan says %v", v, got, want)
		}
	}
	for _, v := range []uint32{1<<31 - 1, 1 << 31, 1<<31 + 5000, 1<<31 + 5001, 0xFFFFFFFF} {
		if got, want := s.Contains(fromUint32(v)), linear(v); got != want {
			t.Fatalf("Contains(%d) = %v, linear scan says %v", v, got, want)
		}
	}
}

const telegramSample = `91.108.56.0/22
91.108.4.0/22
149.154.160.0/20
2001:b28:f23d::/48
2001:67c:4e8::/48
91.108.4.0/22
185.76.151.0/24
`

func TestParsePrefixesKeepsIPv4SortedAndUnique(t *testing.T) {
	got, err := ParsePrefixes(strings.NewReader(telegramSample))
	if err != nil {
		t.Fatalf("ParsePrefixes: %v", err)
	}
	want := []string{"91.108.4.0/22", "91.108.56.0/22", "149.154.160.0/20", "185.76.151.0/24"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, p := range got {
		if p.String() != want[i] {
			t.Errorf("entry %d = %s, want %s", i, p, want[i])
		}
	}
}

func TestParsePrefixesMasksHostBitsAndIgnoresComments(t *testing.T) {
	got, err := ParsePrefixes(strings.NewReader("# telegram\n\n91.108.4.77/22   # host bits set\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "91.108.4.0/22" {
		t.Errorf("got %v, want [91.108.4.0/22]", got)
	}
}

// A list that silently drops an entry would leave part of the traffic outside
// the tunnel, so a bad line has to be an error.
func TestParsePrefixesRejectsBadLines(t *testing.T) {
	for name, in := range map[string]string{
		"not a network": "91.108.4.0/22\nhello\n",
		"no length":     "91.108.4.0\n",
		"empty list":    "# nothing\n",
		"only ipv6":     "2001:b28:f23d::/48\n",
	} {
		if _, err := ParsePrefixes(strings.NewReader(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
