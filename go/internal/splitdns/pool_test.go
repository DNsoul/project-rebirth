package splitdns

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A /29 has 8 addresses, 6 usable (the network and broadcast are skipped),
// which makes full-pool behaviour cheap to reach in a test.
func smallPool(t *testing.T, reserved ...string) *Pool {
	t.Helper()
	var res []netip.Addr
	for _, r := range reserved {
		res = append(res, ip(t, r))
	}
	p, err := NewPool(netip.MustParsePrefix("198.18.0.0/29"), res...)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

func TestNewPoolValidation(t *testing.T) {
	if _, err := NewPool(netip.MustParsePrefix("2001:db8::/64")); err == nil {
		t.Error("an IPv6 range was accepted")
	}
	if _, err := NewPool(netip.MustParsePrefix("198.18.0.0/31")); err == nil {
		t.Error("a range too small to use was accepted")
	}
	if _, err := NewPool(netip.MustParsePrefix("198.18.0.0/29"), ip(t, "10.0.0.1")); err == nil {
		t.Error("a reserved address outside the range was accepted")
	}
}

func TestPoolCapacityExcludesNetworkBroadcastAndReserved(t *testing.T) {
	p := smallPool(t, "198.18.0.1")
	if got := p.Capacity(); got != 5 { // 8 - network - broadcast - 1 reserved
		t.Errorf("Capacity() = %d, want 5", got)
	}
	if got := smallPool(t).Capacity(); got != 6 {
		t.Errorf("Capacity() without reservations = %d, want 6", got)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestAssignIsStablePerRealAddress(t *testing.T) {
	p := smallPool(t)
	a, err := p.Assign(ip(t, "8.8.8.8"))
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if !a.Created {
		t.Error("the first assignment was not reported as created")
	}

	b, err := p.Assign(ip(t, "8.8.8.8"))
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if b.Created {
		t.Error("asking again reported a new mapping")
	}
	if a.Fake != b.Fake {
		t.Errorf("the same real address got %s then %s", a.Fake, b.Fake)
	}

	// An IPv4-mapped form of the same address is the same host.
	c, err := p.Assign(ip(t, "::ffff:8.8.8.8"))
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if c.Fake != a.Fake {
		t.Errorf("a mapped form got a different stand-in: %s vs %s", c.Fake, a.Fake)
	}
	if p.Len() != 1 {
		t.Errorf("Len() = %d, want 1", p.Len())
	}
}

func TestAssignNeverHandsOutReservedNetworkOrBroadcast(t *testing.T) {
	p := smallPool(t, "198.18.0.1")
	got := map[netip.Addr]bool{}
	for i := 0; i < p.Capacity(); i++ {
		a, err := p.Assign(netip.AddrFrom4([4]byte{8, 8, 8, byte(i + 1)}))
		if err != nil {
			t.Fatalf("Assign %d: %v", i, err)
		}
		got[a.Fake] = true
	}
	if len(got) != 5 {
		t.Errorf("got %d distinct stand-ins, want 5", len(got))
	}
	for _, bad := range []string{"198.18.0.0", "198.18.0.1", "198.18.0.7"} {
		if got[ip(t, bad)] {
			t.Errorf("%s was handed out", bad)
		}
	}
}

func TestAssignRejectsIPv6(t *testing.T) {
	p := smallPool(t)
	if _, err := p.Assign(ip(t, "2001:db8::1")); err == nil {
		t.Error("an IPv6 address was accepted")
	}
}

func TestFullPoolEvictsTheLeastRecentlyUsed(t *testing.T) {
	p := smallPool(t)
	clock := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return clock }

	reals := make([]netip.Addr, 0, 6)
	for i := 0; i < 6; i++ {
		r := netip.AddrFrom4([4]byte{8, 8, 8, byte(i + 1)})
		reals = append(reals, r)
		clock = clock.Add(time.Minute)
		if _, err := p.Assign(r); err != nil {
			t.Fatalf("Assign %d: %v", i, err)
		}
	}
	if p.Len() != 6 {
		t.Fatalf("Len() = %d, want 6", p.Len())
	}

	// Touch the oldest, so the next-oldest becomes the one to go.
	clock = clock.Add(time.Minute)
	if _, err := p.Assign(reals[0]); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(time.Minute)
	a, err := p.Assign(ip(t, "9.9.9.9"))
	if err != nil {
		t.Fatalf("Assign into a full pool: %v", err)
	}
	if !a.Created || a.Evicted == nil {
		t.Fatalf("expected an eviction, got %+v", a)
	}
	if a.Evicted.Real != reals[1] {
		t.Errorf("evicted %s, want the least recently used %s", a.Evicted.Real, reals[1])
	}
	if a.Fake != a.Evicted.Fake {
		t.Errorf("the new mapping got %s but recycled %s", a.Fake, a.Evicted.Fake)
	}
	if _, ok := p.Lookup(a.Fake); !ok {
		t.Error("the new mapping is not in the pool")
	}
	if real, _ := p.Lookup(a.Fake); real != ip(t, "9.9.9.9") {
		t.Errorf("the recycled stand-in points at %s", real)
	}
	if p.Len() != 6 {
		t.Errorf("Len() = %d after eviction, want 6", p.Len())
	}
	// The one we touched must have survived.
	if _, err := p.Assign(reals[0]); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseFreesTheSlot(t *testing.T) {
	p := smallPool(t)
	a, err := p.Assign(ip(t, "8.8.8.8"))
	if err != nil {
		t.Fatal(err)
	}
	p.Release(a.Fake)

	if p.Len() != 0 {
		t.Errorf("Len() = %d after release", p.Len())
	}
	if _, ok := p.Lookup(a.Fake); ok {
		t.Error("a released stand-in still resolves")
	}
	b, err := p.Assign(ip(t, "8.8.8.8"))
	if err != nil {
		t.Fatal(err)
	}
	if !b.Created {
		t.Error("re-assigning after release did not create a new mapping")
	}
	p.Release(ip(t, "198.18.0.5")) // releasing something unknown must be harmless
}

func TestExpireRemovesOnlyStaleEntries(t *testing.T) {
	p := smallPool(t)
	clock := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return clock }

	old, err := p.Assign(ip(t, "8.8.8.1"))
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(48 * time.Hour)
	fresh, err := p.Assign(ip(t, "8.8.8.2"))
	if err != nil {
		t.Fatal(err)
	}

	gone := p.Expire(clock.Add(-24 * time.Hour))
	if len(gone) != 1 || gone[0].Fake != old.Fake {
		t.Fatalf("Expire returned %+v, want only the old entry", gone)
	}
	if _, ok := p.Lookup(old.Fake); ok {
		t.Error("the expired entry is still in the pool")
	}
	if _, ok := p.Lookup(fresh.Fake); !ok {
		t.Error("the fresh entry was expired")
	}
}

func TestSnapshotIsOrderedAndRestoreRoundTrips(t *testing.T) {
	p := smallPool(t)
	for i := 1; i <= 4; i++ {
		if _, err := p.Assign(netip.AddrFrom4([4]byte{8, 8, 8, byte(i)})); err != nil {
			t.Fatal(err)
		}
	}
	snap := p.Snapshot()
	for i := 1; i < len(snap); i++ {
		if !snap[i-1].Fake.Less(snap[i].Fake) {
			t.Error("snapshot is not ordered by stand-in address")
		}
	}

	q := smallPool(t)
	if err := q.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if q.Len() != 4 {
		t.Fatalf("restored Len() = %d, want 4", q.Len())
	}
	for _, e := range snap {
		real, ok := q.Lookup(e.Fake)
		if !ok || real != e.Real {
			t.Errorf("%s restored as %s (found=%v), want %s", e.Fake, real, ok, e.Real)
		}
	}

	// A new assignment after restore must not collide with a restored one.
	next, err := q.Assign(ip(t, "9.9.9.9"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range snap {
		if e.Fake == next.Fake {
			t.Errorf("a new assignment reused the restored stand-in %s", e.Fake)
		}
	}
}

// Restore is the way a corrupted file would turn into a wrong translation in
// the kernel, so each way an entry can be wrong has to be refused.
func TestRestoreRefusesBadEntries(t *testing.T) {
	good := Entry{Fake: ip(t, "198.18.0.2"), Real: ip(t, "8.8.8.8")}

	cases := map[string][]Entry{
		"stand-in outside the range": {{Fake: ip(t, "10.0.0.2"), Real: ip(t, "8.8.8.8")}},
		"the network address":        {{Fake: ip(t, "198.18.0.0"), Real: ip(t, "8.8.8.8")}},
		"the broadcast address":      {{Fake: ip(t, "198.18.0.7"), Real: ip(t, "8.8.8.8")}},
		"a reserved address":         {{Fake: ip(t, "198.18.0.1"), Real: ip(t, "8.8.8.8")}},
		"a private real address":     {{Fake: ip(t, "198.18.0.3"), Real: ip(t, "10.0.0.1")}},
		"a metadata real address":    {{Fake: ip(t, "198.18.0.3"), Real: ip(t, "169.254.169.254")}},
		"a duplicate stand-in":       {good, {Fake: ip(t, "198.18.0.2"), Real: ip(t, "9.9.9.9")}},
		"a duplicate real address":   {good, {Fake: ip(t, "198.18.0.3"), Real: ip(t, "8.8.8.8")}},
		"an IPv6 real address":       {{Fake: ip(t, "198.18.0.3"), Real: ip(t, "2001:db8::1")}},
	}
	for name, entries := range cases {
		p := smallPool(t, "198.18.0.1")
		if err := p.Restore(entries); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if p.Len() != 0 {
			t.Errorf("%s: a refused restore left %d entries behind", name, p.Len())
		}
	}
}

func TestSaveAndLoadEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "pool.json")

	if got, err := LoadEntries(path); err != nil || got != nil {
		t.Fatalf("a missing file should load as empty, got %v / %v", got, err)
	}

	want := []Entry{
		{Fake: ip(t, "198.18.0.2"), Real: ip(t, "8.8.8.8"), LastUsed: time.Unix(1_000_000, 0).UTC()},
		{Fake: ip(t, "198.18.0.3"), Real: ip(t, "1.1.1.1"), LastUsed: time.Unix(1_000_100, 0).UTC()},
	}
	if err := SaveEntries(path, want); err != nil {
		t.Fatalf("SaveEntries: %v", err)
	}
	got, err := LoadEntries(path)
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("round trip changed the table:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadEntriesRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.json")
	if err := SaveEntries(path, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(path, "this is not json"); err != nil {
		t.Fatal(err)
	}
	_, err := LoadEntries(path)
	if err == nil {
		t.Fatal("a corrupt table was accepted")
	}
	if !strings.Contains(err.Error(), "pool.json") {
		t.Errorf("error does not name the file: %v", err)
	}
}
