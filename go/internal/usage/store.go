package usage

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// How much is kept. Hours cover "the last day" exactly; days cover a month of
// the provider's rolling count with some room.
const (
	keepHours = 48
	keepDays  = 35

	// Lists are cut to their largest entries, the rest summed as SiteOther,
	// so that the file stays small however many sites are visited.
	maxSitesHour       = 50
	maxDeviceSitesHour = 5
	maxSitesDay        = 100
	maxDeviceSitesDay  = 10
)

// Periods a report can cover.
const (
	Day   = "day"   // the last 24 hours
	Week  = "week"  // today and the six days before, by UTC days
	Month = "month" // today and the 29 days before
)

// Record is traffic of one device to one site.
type Record struct {
	Device string
	Site   string
	Bytes
}

// Bucket is the sums for one hour or one day (UTC).
type Bucket struct {
	Start int64 `json:"start"` // Unix time of the hour or day it begins

	Devices     map[string]Bytes            `json:"devices,omitempty"`
	Sites       map[string]Bytes            `json:"sites,omitempty"`
	DeviceSites map[string]map[string]Bytes `json:"device_sites,omitempty"`

	// Server is the server's own network cards: Down what they received, Up
	// what they sent, which is what the provider counts.
	Server Bytes `json:"server,omitempty"`
}

// History is what is kept on disk.
type History struct {
	Version int       `json:"version"`
	Hours   []*Bucket `json:"hours"`
	Days    []*Bucket `json:"days"`
}

// historyVersion is the layout written.
const historyVersion = 1

// Store is the history and the file it lives in. Safe for concurrent use.
type Store struct {
	path string

	mu    sync.Mutex
	h     History
	dirty bool
}

// Open reads the history, or starts an empty one if there is none. A file
// that cannot be read is an error; the caller decides what to do with it.
func Open(path string) (*Store, error) {
	s := &Store{path: path, h: History{Version: historyVersion}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	h, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.h = h
	return s, nil
}

// Parse checks a history read from elsewhere, such as the one a server hands
// to its replacement.
func Parse(raw []byte) (History, error) {
	var h History
	if err := json.Unmarshal(raw, &h); err != nil {
		return History{}, fmt.Errorf("not a usage history: %w", err)
	}
	if h.Version != historyVersion {
		return History{}, fmt.Errorf("usage history version %d, this build understands %d", h.Version, historyVersion)
	}
	return h, nil
}

func hourOf(t time.Time) int64 { return t.UTC().Truncate(time.Hour).Unix() }

func dayOf(t time.Time) int64 {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix()
}

// bucket returns the bucket that starts at start, making it if needed. The
// list is kept in order, newest last.
func bucket(list *[]*Bucket, start int64) *Bucket {
	l := *list
	if n := len(l); n > 0 && l[n-1].Start == start {
		return l[n-1]
	}
	i, found := slices.BinarySearchFunc(l, start, func(b *Bucket, s int64) int { return cmp.Compare(b.Start, s) })
	if found {
		return l[i]
	}
	b := &Bucket{Start: start}
	*list = slices.Insert(l, i, b)
	return b
}

func (b *Bucket) add(r Record) {
	if b.Devices == nil {
		b.Devices, b.Sites, b.DeviceSites = map[string]Bytes{}, map[string]Bytes{}, map[string]map[string]Bytes{}
	}
	d := b.Devices[r.Device]
	d.add(r.Bytes)
	b.Devices[r.Device] = d

	s := b.Sites[r.Site]
	s.add(r.Bytes)
	b.Sites[r.Site] = s

	ds := b.DeviceSites[r.Device]
	if ds == nil {
		ds = map[string]Bytes{}
		b.DeviceSites[r.Device] = ds
	}
	v := ds[r.Site]
	v.add(r.Bytes)
	ds[r.Site] = v
}

// Add counts traffic at a moment.
func (s *Store) Add(at time.Time, recs []Record) {
	if len(recs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hour, day := bucket(&s.h.Hours, hourOf(at)), bucket(&s.h.Days, dayOf(at))
	for _, r := range recs {
		if r.Total() == 0 {
			continue
		}
		hour.add(r)
		day.add(r)
	}
	s.dirty = true
}

// AddServer counts what the server's network cards received and sent.
func (s *Store) AddServer(at time.Time, b Bytes) {
	if b.Total() == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket(&s.h.Hours, hourOf(at)).Server.add(b)
	bucket(&s.h.Days, dayOf(at)).Server.add(b)
	s.dirty = true
}

// prune drops what is too old and cuts long lists.
func (s *Store) prune(now time.Time) {
	oldHour := hourOf(now) - (keepHours-1)*3600
	s.h.Hours = slices.DeleteFunc(s.h.Hours, func(b *Bucket) bool { return b.Start < oldHour })
	oldDay := time.Unix(dayOf(now), 0).AddDate(0, 0, -(keepDays - 1)).Unix()
	s.h.Days = slices.DeleteFunc(s.h.Days, func(b *Bucket) bool { return b.Start < oldDay })
	for _, b := range s.h.Hours {
		b.trim(maxSitesHour, maxDeviceSitesHour)
	}
	for _, b := range s.h.Days {
		b.trim(maxSitesDay, maxDeviceSitesDay)
	}
}

func (b *Bucket) trim(sites, perDevice int) {
	b.Sites = cut(b.Sites, sites)
	for d, m := range b.DeviceSites {
		b.DeviceSites[d] = cut(m, perDevice)
	}
}

// cut keeps the largest n-1 entries of m and sums the rest as SiteOther.
func cut(m map[string]Bytes, n int) map[string]Bytes {
	if len(m) <= n {
		return m
	}
	items := sorted(m)
	out := make(map[string]Bytes, n)
	var rest Bytes
	kept := 0
	for _, it := range items {
		if it.Key != SiteOther && kept < n-1 {
			out[it.Key] = it.Bytes
			kept++
			continue
		}
		rest.add(it.Bytes)
	}
	out[SiteOther] = rest
	return out
}

// Reset forgets the whole history, for the operator who wants counting to
// start again, and writes the empty history at once so a restart does not
// bring the old one back.
func (s *Store) Reset(now time.Time) error {
	s.mu.Lock()
	s.h = History{Version: historyVersion}
	s.dirty = true
	s.mu.Unlock()
	return s.Save(now)
}

// Save writes the history if anything changed since it was last written,
// first dropping what is too old.
func (s *Store) Save(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	if !s.dirty {
		return nil
	}
	raw, err := json.Marshal(s.h)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Item is one line of a report.
type Item struct {
	Key string `json:"key"`
	Bytes
}

// Report is the sums over a period, the largest first.
type Report struct {
	Period string `json:"period"`

	// From is where the period begins, or where counting began if that was
	// later.
	From int64 `json:"from"`

	Total   Bytes  `json:"total"`
	Server  Bytes  `json:"server"`
	Sites   []Item `json:"sites"`
	Devices []Item `json:"devices,omitempty"`

	// Device is set for a report about one device; Devices is then empty.
	Device string `json:"device,omitempty"`

	// Site is set for a report about one site: Total is the traffic to it,
	// Devices the devices that used it, and Sites is empty. A device's own
	// list is cut shorter than the overall one, so the devices of a small
	// site may add up to less than its total.
	Site string `json:"site,omitempty"`
}

// ValidPeriod reports whether p is a period a report can cover.
func ValidPeriod(p string) bool { return p == Day || p == Week || p == Month }

// span picks the buckets a period is summed from and where it begins.
func (s *Store) span(period string, now time.Time) ([]*Bucket, int64, error) {
	switch period {
	case Day:
		return s.h.Hours, hourOf(now) - 23*3600, nil
	case Week:
		return s.h.Days, time.Unix(dayOf(now), 0).AddDate(0, 0, -6).Unix(), nil
	case Month:
		return s.h.Days, time.Unix(dayOf(now), 0).AddDate(0, 0, -29).Unix(), nil
	}
	return nil, 0, fmt.Errorf("unknown period %q", period)
}

// Report sums a period, for every device or for one.
func (s *Store) Report(period, device string, now time.Time) (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, start, err := s.span(period, now)
	if err != nil {
		return Report{}, err
	}

	r := Report{Period: period, Device: device, From: start}
	sites, devices := map[string]Bytes{}, map[string]Bytes{}
	first := true
	for _, b := range list {
		if b.Start < start {
			continue
		}
		if first {
			r.From = max(start, b.Start)
			first = false
		}
		r.Server.add(b.Server)
		if device != "" {
			r.Total.add(b.Devices[device])
			for k, v := range b.DeviceSites[device] {
				x := sites[k]
				x.add(v)
				sites[k] = x
			}
			continue
		}
		for k, v := range b.Devices {
			r.Total.add(v)
			x := devices[k]
			x.add(v)
			devices[k] = x
		}
		for k, v := range b.Sites {
			x := sites[k]
			x.add(v)
			sites[k] = x
		}
	}
	r.Sites = sorted(sites)
	if device == "" {
		r.Devices = sorted(devices)
	}
	return r, nil
}

// SiteReport sums a period for one site: how much went to it, and from which
// devices.
func (s *Store) SiteReport(period, site string, now time.Time) (Report, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, start, err := s.span(period, now)
	if err != nil {
		return Report{}, err
	}

	r := Report{Period: period, Site: site, From: start}
	devices := map[string]Bytes{}
	first := true
	for _, b := range list {
		if b.Start < start {
			continue
		}
		if first {
			r.From = max(start, b.Start)
			first = false
		}
		r.Server.add(b.Server)
		r.Total.add(b.Sites[site])
		for d, m := range b.DeviceSites {
			if v, ok := m[site]; ok {
				x := devices[d]
				x.add(v)
				devices[d] = x
			}
		}
	}
	r.Devices = sorted(devices)
	return r, nil
}

// Snapshot returns the history as it would be written, for a replacement.
func (s *Store) Snapshot(now time.Time) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	return json.Marshal(s.h)
}

// sorted lists a map largest first, by name when equal.
func sorted(m map[string]Bytes) []Item {
	out := make([]Item, 0, len(m))
	for k, v := range m {
		out = append(out, Item{Key: k, Bytes: v})
	}
	slices.SortFunc(out, func(a, b Item) int {
		if c := cmp.Compare(b.Total(), a.Total()); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	return out
}
