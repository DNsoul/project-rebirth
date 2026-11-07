package enroll

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/proto"
)

func testPool(t *testing.T) Pool {
	t.Helper()
	p, err := NewPool("10.8.0.0/24", 2, 249)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func b64(b byte, n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = b
	}
	return proto.Encode(buf)
}

func request(id, key byte, name string, now time.Time) *proto.EnrollRequest {
	return &proto.EnrollRequest{
		V: proto.FormatVersion, Kind: proto.KindEnroll,
		DeviceID: b64(id, proto.DeviceIDSize), PublicKey: b64(key, proto.TunnelKeySize),
		Nonce: b64(0xAA, proto.EnrollNonceSize), IssuedAt: now.Unix(), Name: name,
	}
}

func enrollOK(t *testing.T, r *Registry, req *proto.EnrollRequest, now time.Time) Device {
	t.Helper()
	plan, refusal := r.Plan(req, now)
	if refusal != "" {
		t.Fatalf("refused: %s", refusal)
	}
	if err := r.Commit(plan); err != nil {
		t.Fatal(err)
	}
	return plan.Device
}

func TestPoolRejectsWhatItCannotUse(t *testing.T) {
	for _, c := range []struct {
		subnet   string
		from, to int
	}{
		{"10.8.0.0/16", 2, 249}, {"fd00::/64", 2, 249}, {"nonsense", 2, 249},
		{"10.8.0.0/24", 0, 249}, {"10.8.0.0/24", 2, 255}, {"10.8.0.0/24", 9, 3},
	} {
		if _, err := NewPool(c.subnet, c.from, c.to); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestAddressesAreHandedOutInOrderAndStayTheSame(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := NewRegistry(filepath.Join(t.TempDir(), "r.json"), testPool(t), Options{})

	a := enrollOK(t, r, request(1, 1, "windows", now), now)
	b := enrollOK(t, r, request(2, 2, "phone", now), now)
	if a.Address != "10.8.0.2/32" || b.Address != "10.8.0.3/32" {
		t.Fatalf("addresses = %s, %s", a.Address, b.Address)
	}

	// The same device again, same key: nothing changes.
	again := enrollOK(t, r, request(1, 1, "windows", now.Add(time.Hour)), now.Add(time.Hour))
	if again.Address != a.Address || r.Len() != 2 {
		t.Errorf("a repeat changed things: %s, %d devices", again.Address, r.Len())
	}
}

func TestANewKeyForTheSameDeviceKeepsItsAddressAndDropsTheOldKey(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := NewRegistry(filepath.Join(t.TempDir(), "r.json"), testPool(t), Options{})
	enrollOK(t, r, request(1, 1, "windows", now), now)
	first := enrollOK(t, r, request(2, 2, "phone", now), now)

	plan, refusal := r.Plan(request(2, 9, "phone", now), now)
	if refusal != "" {
		t.Fatal(refusal)
	}
	if plan.Device.Address != first.Address {
		t.Errorf("address changed from %s to %s", first.Address, plan.Device.Address)
	}
	if len(plan.Drop) != 1 || plan.Drop[0] != b64(2, proto.TunnelKeySize) {
		t.Errorf("drop = %v, want the old key", plan.Drop)
	}
}

func TestAKeyBelongsToOneDevice(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := NewRegistry(filepath.Join(t.TempDir(), "r.json"), testPool(t), Options{})
	enrollOK(t, r, request(1, 1, "windows", now), now)

	if _, refusal := r.Plan(request(2, 1, "clone", now), now); refusal != proto.EnrollKeyInUse {
		t.Errorf("a second device with the same key: %q", refusal)
	}
	// And an existing device cannot take over another's key by "changing" to it.
	enrollOK(t, r, request(2, 2, "phone", now), now)
	if _, refusal := r.Plan(request(2, 1, "phone", now), now); refusal != proto.EnrollKeyInUse {
		t.Errorf("changing to another device's key: %q", refusal)
	}
}

func TestFullRegistryRefusesUnlessSomeoneHasGoneStale(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	r := NewRegistry(filepath.Join(t.TempDir(), "r.json"), testPool(t),
		Options{MaxDevices: 2, StaleAfter: 30 * 24 * time.Hour})
	enrollOK(t, r, request(1, 1, "old", t0), t0)
	enrollOK(t, r, request(2, 2, "new", t0.Add(20*24*time.Hour)), t0.Add(20*24*time.Hour))

	now := t0.Add(25 * 24 * time.Hour) // nobody has been gone for 30 days yet
	if _, refusal := r.Plan(request(3, 3, "third", now), now); refusal != proto.EnrollFull {
		t.Fatalf("a full registry took a third device: %q", refusal)
	}

	now = t0.Add(31 * 24 * time.Hour) // "old" has now been gone for 31 days
	plan, refusal := r.Plan(request(3, 3, "third", now), now)
	if refusal != "" {
		t.Fatalf("refused although one device is stale: %s", refusal)
	}
	if plan.Device.Address != "10.8.0.2/32" {
		t.Errorf("the freed address was not reused: %s", plan.Device.Address)
	}
	if len(plan.Drop) != 1 || plan.Drop[0] != b64(1, proto.TunnelKeySize) {
		t.Errorf("drop = %v, want the stale device's key", plan.Drop)
	}
	if err := r.Commit(plan); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 2 {
		t.Errorf("%d devices after the swap", r.Len())
	}
	for _, d := range r.Devices() {
		if d.Name == "old" {
			t.Error("the stale device is still registered")
		}
	}
}

func TestRegistrySurvivesARestart(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	path := filepath.Join(t.TempDir(), "sub", "r.json")
	pool := testPool(t)
	r := NewRegistry(path, pool, Options{})
	enrollOK(t, r, request(1, 1, "windows", now), now)
	enrollOK(t, r, request(2, 2, "phone", now), now)

	again, err := LoadRegistry(path, pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := again.Devices()
	if len(got) != 2 || got[0].Name != "windows" || got[1].Address != "10.8.0.3/32" {
		t.Errorf("after reload: %+v", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o077 != 0 && !isWindows() {
		t.Errorf("registry file is readable by others: %v %v", info, err)
	}
}

func isWindows() bool { return os.PathSeparator == '\\' }

func TestAMissingFileIsAnEmptyRegistry(t *testing.T) {
	r, err := LoadRegistry(filepath.Join(t.TempDir(), "none.json"), testPool(t), Options{})
	if err != nil || r.Len() != 0 {
		t.Fatalf("%v, %d devices", err, r.Len())
	}
}

func TestBrokenFilesAreRefusedNotHalfRead(t *testing.T) {
	pool := testPool(t)
	good := func(extra string) string {
		return `{"version":1,"devices":[` + extra + `]}`
	}
	dev := func(id, key byte, addr string) string {
		return `{"id":"` + b64(id, proto.DeviceIDSize) + `","pub":"` + b64(key, proto.TunnelKeySize) +
			`","address":"` + addr + `","enrolled_at":1,"last_seen":1}`
	}
	cases := map[string]string{
		"not json":          `{`,
		"wrong version":     `{"version":9,"devices":[]}`,
		"address outside":   good(dev(1, 1, "10.8.0.250/32")),
		"address elsewhere": good(dev(1, 1, "192.168.0.5/32")),
		"repeated id":       good(dev(1, 1, "10.8.0.2/32") + "," + dev(1, 2, "10.8.0.3/32")),
		"repeated key":      good(dev(1, 1, "10.8.0.2/32") + "," + dev(2, 1, "10.8.0.3/32")),
		"repeated address":  good(dev(1, 1, "10.8.0.2/32") + "," + dev(2, 2, "10.8.0.2/32")),
		"short key":         good(`{"id":"` + b64(1, 16) + `","pub":"AAAA","address":"10.8.0.2/32"}`),
	}
	for name, content := range cases {
		path := filepath.Join(t.TempDir(), "r.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadRegistry(path, pool, Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTouchIsSavedOnFlush(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	path := filepath.Join(t.TempDir(), "r.json")
	pool := testPool(t)
	r := NewRegistry(path, pool, Options{})
	d := enrollOK(t, r, request(1, 1, "windows", t0), t0)

	seen := t0.Add(3 * time.Hour)
	r.Touch(d.PublicKey, seen)
	r.Touch(d.PublicKey, t0) // an older time never moves it back
	if err := r.Flush(); err != nil {
		t.Fatal(err)
	}
	again, err := LoadRegistry(path, pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Devices()[0].LastSeen; got != seen.Unix() {
		t.Errorf("last seen = %d, want %d", got, seen.Unix())
	}
}

func TestACommitThatCannotBeSavedChangesNothing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	dir := t.TempDir()
	r := NewRegistry(filepath.Join(dir, "r.json"), testPool(t), Options{})
	enrollOK(t, r, request(1, 1, "windows", now), now)

	// A directory where the temporary file should go makes writing fail.
	if err := os.Mkdir(filepath.Join(dir, "r.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan, refusal := r.Plan(request(2, 2, "phone", now), now)
	if refusal != "" {
		t.Fatal(refusal)
	}
	err := r.Commit(plan)
	if err == nil || !strings.Contains(err.Error(), "r.json.tmp") {
		t.Fatalf("commit error = %v", err)
	}
	if r.Len() != 1 {
		t.Errorf("%d devices after a failed commit, want 1", r.Len())
	}
}
