package splitdns

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry maps one stand-in address to the real address it stands for.
//
// The mapping is keyed by the real address, not by the name. Two names that
// resolve to the same server share a stand-in, which keeps the table small and
// means a client that cached a stand-in keeps working however the name moves.
type Entry struct {
	Fake     netip.Addr `json:"fake"`
	Real     netip.Addr `json:"real"`
	LastUsed time.Time  `json:"last_used"`
}

// Assignment is the result of asking for a stand-in.
type Assignment struct {
	Fake netip.Addr

	// Created is true when the mapping is new, which is when the caller has
	// to install it in the kernel.
	Created bool

	// Evicted is set when the pool was full and the least recently used
	// mapping was recycled. The caller must remove it from the kernel before
	// installing the new one, or traffic for the old destination would
	// briefly be delivered to the new one.
	Evicted *Entry
}

// Pool hands out stand-in addresses from a range and remembers what each
// stands for.
type Pool struct {
	mu       sync.Mutex
	base     uint32 // numeric value of the range's first address
	usable   uint32 // how many addresses can be handed out
	reserved map[netip.Addr]bool
	byReal   map[netip.Addr]*Entry
	byFake   map[netip.Addr]*Entry
	cursor   uint32 // where the next free-address search starts
	now      func() time.Time
}

// NewPool builds a pool over a range. The network and broadcast addresses are
// never handed out, nor are any reserved addresses (the resolver's own, for
// one).
func NewPool(prefix netip.Prefix, reserved ...netip.Addr) (*Pool, error) {
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("%s is not an IPv4 range", prefix)
	}
	if prefix.Bits() > 30 {
		return nil, fmt.Errorf("%s is too small to hand addresses out of", prefix)
	}

	size := uint32(1) << (32 - prefix.Bits())
	p := &Pool{
		base:     addrToUint32(prefix.Addr()),
		usable:   size - 2,
		reserved: map[netip.Addr]bool{},
		byReal:   map[netip.Addr]*Entry{},
		byFake:   map[netip.Addr]*Entry{},
		now:      time.Now,
	}
	for _, r := range reserved {
		if !prefix.Contains(r) {
			return nil, fmt.Errorf("reserved address %s is outside %s", r, prefix)
		}
		p.reserved[r] = true
	}
	return p, nil
}

// Capacity is how many mappings the pool can hold.
func (p *Pool) Capacity() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int(p.usable) - len(p.reserved)
}

// Len is how many mappings are in use.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byReal)
}

// Assign returns the stand-in for a real address, creating one if needed.
// Asking again for the same address returns the same stand-in and counts as a
// use.
func (p *Pool) Assign(real netip.Addr) (Assignment, error) {
	real = real.Unmap()
	if !real.Is4() {
		return Assignment{}, fmt.Errorf("%s is not an IPv4 address", real)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if e, ok := p.byReal[real]; ok {
		e.LastUsed = p.now()
		return Assignment{Fake: e.Fake}, nil
	}

	// Look for a free slot, starting where the last search ended so addresses
	// are used in rotation rather than the low ones being recycled first.
	for i := uint32(0); i < p.usable; i++ {
		idx := (p.cursor + i) % p.usable
		fake := uint32ToAddr(p.base + 1 + idx)
		if p.reserved[fake] {
			continue
		}
		if _, taken := p.byFake[fake]; taken {
			continue
		}
		p.cursor = (idx + 1) % p.usable
		p.store(fake, real)
		return Assignment{Fake: fake, Created: true}, nil
	}

	// Full: recycle the entry that has gone longest without being asked for.
	var oldest *Entry
	for _, e := range p.byFake {
		if oldest == nil || e.LastUsed.Before(oldest.LastUsed) {
			oldest = e
		}
	}
	if oldest == nil {
		return Assignment{}, errors.New("the stand-in range has no usable addresses")
	}
	evicted := *oldest
	p.remove(oldest)
	p.store(evicted.Fake, real)
	return Assignment{Fake: evicted.Fake, Created: true, Evicted: &evicted}, nil
}

// Lookup returns the real address behind a stand-in.
func (p *Pool) Lookup(fake netip.Addr) (netip.Addr, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byFake[fake]
	if !ok {
		return netip.Addr{}, false
	}
	return e.Real, true
}

// Release drops a mapping. It is used when installing the mapping in the
// kernel failed: keeping it would hand out a stand-in that goes nowhere.
func (p *Pool) Release(fake netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byFake[fake]; ok {
		p.remove(e)
	}
}

// Expire removes every mapping not used since the cutoff and returns them, so
// the caller can remove them from the kernel too.
func (p *Pool) Expire(cutoff time.Time) []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()

	var gone []Entry
	for _, e := range p.byFake {
		if e.LastUsed.Before(cutoff) {
			gone = append(gone, *e)
		}
	}
	for _, e := range gone {
		if cur, ok := p.byFake[e.Fake]; ok {
			p.remove(cur)
		}
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i].Fake.Less(gone[j].Fake) })
	return gone
}

// Snapshot returns every mapping, ordered by stand-in address so that two
// snapshots of the same state are byte-identical.
func (p *Pool) Snapshot() []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]Entry, 0, len(p.byFake))
	for _, e := range p.byFake {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fake.Less(out[j].Fake) })
	return out
}

// Restore loads mappings saved earlier. It refuses anything that does not fit
// the pool, because a bad entry here would later become a wrong translation
// in the kernel.
func (p *Pool) Restore(entries []Entry) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	seenFake := map[netip.Addr]bool{}
	seenReal := map[netip.Addr]bool{}
	for i, e := range entries {
		fake, real := e.Fake.Unmap(), e.Real.Unmap()
		offset := int64(addrToUint32(fake)) - int64(p.base)
		switch {
		case !fake.Is4() || !real.Is4():
			return fmt.Errorf("entry %d: not an IPv4 mapping", i)
		case offset < 1 || offset > int64(p.usable):
			return fmt.Errorf("entry %d: %s is outside the stand-in range", i, fake)
		case p.reserved[fake]:
			return fmt.Errorf("entry %d: %s is reserved", i, fake)
		case !IsPublic(real):
			return fmt.Errorf("entry %d: %s is not a public address", i, real)
		case seenFake[fake]:
			return fmt.Errorf("entry %d: %s appears twice", i, fake)
		case seenReal[real]:
			return fmt.Errorf("entry %d: %s is mapped twice", i, real)
		}
		seenFake[fake], seenReal[real] = true, true
	}

	p.byReal = make(map[netip.Addr]*Entry, len(entries))
	p.byFake = make(map[netip.Addr]*Entry, len(entries))
	for _, e := range entries {
		entry := &Entry{Fake: e.Fake.Unmap(), Real: e.Real.Unmap(), LastUsed: e.LastUsed}
		p.byReal[entry.Real] = entry
		p.byFake[entry.Fake] = entry
	}
	// Continue after the highest address in use.
	p.cursor = 0
	for _, e := range p.byFake {
		if idx := addrToUint32(e.Fake) - p.base; idx > p.cursor {
			p.cursor = idx % p.usable
		}
	}
	return nil
}

func (p *Pool) store(fake, real netip.Addr) {
	e := &Entry{Fake: fake, Real: real, LastUsed: p.now()}
	p.byReal[real] = e
	p.byFake[fake] = e
}

func (p *Pool) remove(e *Entry) {
	delete(p.byReal, e.Real)
	delete(p.byFake, e.Fake)
}

// SaveEntries writes mappings to a file, replacing it atomically so a crash
// mid-write cannot leave a truncated table.
func SaveEntries(path string, entries []Entry) error {
	raw, err := json.MarshalIndent(entries, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadEntries reads what SaveEntries wrote. A missing file is not an error:
// it is how a fresh server starts.
func LoadEntries(path string) ([]Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%s is not a valid mapping table: %w", path, err)
	}
	return entries, nil
}

func addrToUint32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uint32ToAddr(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
