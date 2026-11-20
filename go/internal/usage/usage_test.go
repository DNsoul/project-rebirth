package usage

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// packet makes an IPv4 packet of the given size from src to dst.
func packet(src, dst string, size int) []byte {
	p := make([]byte, size)
	p[0] = 0x45
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	return p
}

func TestTheMeterCountsEachFlowBothWays(t *testing.T) {
	m := NewMeter()
	const off = 16
	withOffset := func(p []byte) []byte { return append(make([]byte, off), p...) }

	// From the device: written to the tunnel device, whole buffers.
	m.Count([][]byte{
		withOffset(packet("10.8.0.5", "198.18.0.7", 100)),
		withOffset(packet("10.8.0.5", "198.18.0.7", 60)),
		withOffset(packet("10.8.0.9", "149.154.167.50", 80)),
	}, nil, off, 3, false)

	// To the device: read from the tunnel device into larger buffers, with
	// the lengths in sizes. The third buffer was not filled this time.
	big := func(p []byte) []byte { return append(withOffset(p), make([]byte, 500)...) }
	m.Count([][]byte{
		big(packet("198.18.0.7", "10.8.0.5", 1400)),
		big([]byte{0x60, 0, 0, 0}), // IPv6 or garbage: not counted
		big(packet("198.18.0.7", "10.8.0.5", 999)),
	}, []int{1400, 4, 999}, off, 2, true)

	got := m.Drain()
	a := got[Flow{netip.MustParseAddr("10.8.0.5"), netip.MustParseAddr("198.18.0.7")}]
	if a != (Bytes{Down: 1400, Up: 160}) {
		t.Errorf("device 5 = %+v", a)
	}
	b := got[Flow{netip.MustParseAddr("10.8.0.9"), netip.MustParseAddr("149.154.167.50")}]
	if b != (Bytes{Up: 80}) || len(got) != 2 {
		t.Errorf("flows = %+v", got)
	}
	if len(m.Drain()) != 0 {
		t.Error("drain did not start again")
	}
}

func TestTheMeterIsSafeFromManyGoroutines(t *testing.T) {
	m := NewMeter()
	p := [][]byte{packet("10.8.0.5", "198.18.0.7", 100)}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				m.Count(p, nil, 0, 1, false)
			}
		}()
	}
	wg.Wait()
	var total uint64
	for _, b := range m.Drain() {
		total += b.Up
	}
	if total != 8*1000*100 {
		t.Errorf("counted %d", total)
	}
}

func TestSitesAreRegistrableDomainsNeverFullNames(t *testing.T) {
	for name, want := range map[string]string{
		"rr3---sn-4g5e6nzz.googlevideo.com.": "googlevideo.com",
		"www.YouTube.com":                    "youtube.com",
		"cache1-fra1.steamcontent.com":       "steamcontent.com",
		"video-weaver.fra05.hls.ttvnw.net":   "ttvnw.net",
		"cdn.example.co.uk":                  "example.co.uk",
		"a.b.c.github.io":                    "c.github.io",
		"img.dgeft87wbj63p.cloudfront.net":   "dgeft87wbj63p.cloudfront.net",
		"localhost":                          "localhost",
		"":                                   SiteUnknown,
	} {
		if got := SiteOf(name); got != want {
			t.Errorf("SiteOf(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestAKnownDomainNamesItsService(t *testing.T) {
	if ServiceOf("googlevideo.com") != "YouTube" || ServiceOf("steamcontent.com") != "Steam" || ServiceOf("example.com") != "" {
		t.Error(ServiceOf("googlevideo.com"), ServiceOf("steamcontent.com"), ServiceOf("example.com"))
	}
}

func TestEachAddressIsGivenItsSite(t *testing.T) {
	c := Classifier{
		StandIns: netip.MustParsePrefix("198.18.0.0/15"),
		Names: func(a netip.Addr) string {
			if a == netip.MustParseAddr("198.18.0.7") {
				return "YouTube"
			}
			return ""
		},
		Server:   []netip.Prefix{netip.MustParsePrefix("10.8.0.0/24"), netip.MustParsePrefix("198.18.0.1/32")},
		Telegram: []netip.Prefix{netip.MustParsePrefix("149.154.160.0/20")},
	}
	for addr, want := range map[string]string{
		"198.18.0.7": "YouTube", "198.18.3.3": SiteUnknown, "10.8.0.1": SiteServer,
		"149.154.167.50": SiteTelegram, "8.8.8.8": SiteElsewhere,
	} {
		if got := c.Site(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s = %q, want %q", addr, got, want)
		}
	}

	devices := map[string]string{"10.8.0.5": "phone", "10.8.0.9": "laptop"}
	recs := Records(map[Flow]Bytes{
		{netip.MustParseAddr("10.8.0.5"), netip.MustParseAddr("198.18.0.7")}:    {Down: 100},
		{netip.MustParseAddr("10.8.0.5"), netip.MustParseAddr("198.18.3.3")}:    {Down: 5},
		{netip.MustParseAddr("10.8.0.5"), netip.MustParseAddr("198.18.3.4")}:    {Down: 7},
		{netip.MustParseAddr("10.8.0.251"), netip.MustParseAddr("10.8.0.1")}:    {Up: 9},
		{netip.MustParseAddr("10.8.0.9"), netip.MustParseAddr("149.154.167.5")}: {Up: 3},
	}, func(a netip.Addr) (string, bool) { id, ok := devices[a.String()]; return id, ok }, c)
	sums := map[[2]string]Bytes{}
	for _, r := range recs {
		sums[[2]string{r.Device, r.Site}] = r.Bytes
	}
	want := map[[2]string]Bytes{
		{"phone", "YouTube"}: {Down: 100}, {"phone", SiteUnknown}: {Down: 12}, {"laptop", SiteTelegram}: {Up: 3},
	}
	if len(sums) != len(want) {
		t.Fatalf("records = %+v", recs)
	}
	for k, v := range want {
		if sums[k] != v {
			t.Errorf("%v = %+v, want %+v", k, sums[k], v)
		}
	}
}

var t0 = time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)

func TestReportsSumTheLastDayWeekAndMonth(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	gb := uint64(1e9)
	// Ten days back, two days back, 25 hours back, and in the last hour.
	s.Add(t0.AddDate(0, 0, -10), []Record{{Device: "pc", Site: "Steam", Bytes: Bytes{Down: 50 * gb}}})
	s.Add(t0.AddDate(0, 0, -2), []Record{{Device: "pc", Site: "YouTube", Bytes: Bytes{Down: 20 * gb}}})
	s.Add(t0.Add(-25*time.Hour), []Record{{Device: "phone", Site: "YouTube", Bytes: Bytes{Down: 3 * gb}}})
	s.Add(t0.Add(-10*time.Minute), []Record{
		{Device: "pc", Site: "YouTube", Bytes: Bytes{Down: 7 * gb, Up: gb}},
		{Device: "phone", Site: "Telegram", Bytes: Bytes{Down: gb}},
	})
	s.AddServer(t0.Add(-5*time.Minute), Bytes{Down: 10 * gb, Up: 11 * gb})

	day, _ := s.Report(Day, "", t0)
	if day.Total.Total() != 9*gb || day.Server.Up != 11*gb || day.Sites[0].Key != "YouTube" || day.Devices[0].Key != "pc" {
		t.Errorf("day = %+v", day)
	}
	week, _ := s.Report(Week, "", t0)
	if week.Total.Total() != 32*gb {
		t.Errorf("week total = %d", week.Total.Total())
	}
	month, _ := s.Report(Month, "", t0)
	if month.Total.Total() != 82*gb || month.Sites[0].Key != "Steam" || month.From != dayOf(t0.AddDate(0, 0, -10)) {
		t.Errorf("month = %+v", month)
	}
	pc, _ := s.Report(Month, "pc", t0)
	if pc.Total.Total() != 78*gb || len(pc.Devices) != 0 || len(pc.Sites) != 2 || pc.Sites[0].Key != "Steam" {
		t.Errorf("one device = %+v", pc)
	}
	if _, err := s.Report("year", "", t0); err == nil {
		t.Error("an unknown period was accepted")
	}
}

func TestTheHistoryIsKeptSmallAndSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, _ := Open(path)
	var recs []Record
	for i := range 300 {
		recs = append(recs, Record{Device: "pc", Site: "site" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Bytes: Bytes{Down: uint64(1000 + i)}})
	}
	s.Add(t0.AddDate(0, 0, -40), recs[:1]) // too old to keep
	s.Add(t0, recs)
	if err := s.Save(t0); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	month, _ := again.Report(Month, "", t0)
	if len(month.Sites) != maxSitesDay {
		t.Errorf("%d sites kept for the day, want %d", len(month.Sites), maxSitesDay)
	}
	var total uint64
	for _, it := range month.Sites {
		total += it.Total()
	}
	if want := month.Total.Total(); total != want || want == 0 {
		t.Errorf("sites sum to %d, devices to %d: the cut lost traffic", total, want)
	}
	day, _ := again.Report(Day, "", t0)
	if len(day.Sites) != maxSitesHour {
		t.Errorf("%d sites kept for the hour", len(day.Sites))
	}
	if month.From != dayOf(t0) {
		t.Errorf("the 40-day-old day was kept: from %v", time.Unix(month.From, 0))
	}
	pc, _ := again.Report(Day, "pc", t0)
	if len(pc.Sites) != maxDeviceSitesHour {
		t.Errorf("%d sites kept for one device", len(pc.Sites))
	}

	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("mode %v", info.Mode())
	}
	raw, _ := again.Snapshot(t0)
	if _, err := Parse(raw); err != nil {
		t.Errorf("a snapshot does not parse: %v", err)
	}
	if _, err := Parse([]byte(`{"version":9}`)); err == nil {
		t.Error("a future version was accepted")
	}
}

// The meter sits on the path of every packet; a batch of 128 must cost
// microseconds, not more.
func BenchmarkMeterBatch(b *testing.B) {
	m := NewMeter()
	var batch [][]byte
	for i := range 128 {
		batch = append(batch, packet("10.8.0.5", "198.18.0."+string(rune('1'+i%9)), 1280))
	}
	sizes := make([]int, len(batch))
	for i := range sizes {
		sizes[i] = 1280
	}
	b.ResetTimer()
	for range b.N {
		m.Count(batch, sizes, 0, len(batch), true)
	}
}

func TestASiteReportSaysWhichDevicesUsedIt(t *testing.T) {
	const gb = 1_000_000_000
	s, err := Open(t.TempDir() + "/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 20, 12, 30, 0, 0, time.UTC)
	s.Add(t0.Add(-2*time.Hour), []Record{
		{Device: "pc", Site: "steamcontent.com", Bytes: Bytes{Down: 5 * gb}},
		{Device: "phone", Site: "steamcontent.com", Bytes: Bytes{Down: gb}},
		{Device: "phone", Site: "googlevideo.com", Bytes: Bytes{Down: 2 * gb}},
	})
	s.Add(t0.AddDate(0, 0, -3), []Record{{Device: "laptop", Site: "steamcontent.com", Bytes: Bytes{Down: 9 * gb}}})

	day, err := s.SiteReport(Day, "steamcontent.com", t0)
	if err != nil {
		t.Fatal(err)
	}
	if day.Site != "steamcontent.com" || day.Total.Total() != 6*gb || len(day.Devices) != 2 ||
		day.Devices[0].Key != "pc" || day.Devices[1].Key != "phone" || len(day.Sites) != 0 {
		t.Errorf("day: %+v", day)
	}
	week, _ := s.SiteReport(Week, "steamcontent.com", t0)
	if week.Total.Total() != 15*gb || week.Devices[0].Key != "laptop" {
		t.Errorf("week: %+v", week)
	}
	none, _ := s.SiteReport(Day, "example.com", t0)
	if none.Total.Total() != 0 || len(none.Devices) != 0 {
		t.Errorf("unknown site: %+v", none)
	}
	if _, err := s.SiteReport("year", "example.com", t0); err == nil {
		t.Error("a year is not a period")
	}
}
