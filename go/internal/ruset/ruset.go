// Package ruset holds the set of IPv4 ranges delegated to one country and
// answers the only question the splitter asks of it: is this address in it.
//
// The set decides whether a site is reached directly or through the tunnel,
// so two properties matter more than speed. It must be built from a primary
// source rather than a curated list, and a lookup must never be approximately
// right: a false "yes" sends traffic out through the home connection that was
// meant to be tunnelled.
//
// Only IPv4 is covered. The split works on A records, and IPv6 inside the
// tunnel is handled separately.
package ruset

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Set is an immutable, sorted, non-overlapping set of inclusive ranges.
type Set struct {
	starts []uint32
	ends   []uint32
}

// rng is one inclusive range while the set is being built.
type rng struct{ start, end uint32 }

// Builder accumulates ranges and merges them once.
type Builder struct {
	ranges []rng
}

// AddRange adds an inclusive range.
func (b *Builder) AddRange(start, end uint32) error {
	if end < start {
		return fmt.Errorf("range %s-%s runs backwards", fromUint32(start), fromUint32(end))
	}
	b.ranges = append(b.ranges, rng{start, end})
	return nil
}

// AddCount adds a range given as a first address and a count, which is how
// RIR statistics files describe IPv4 delegations. The count is not always a
// power of two and the range is not always aligned, so this cannot be treated
// as a prefix.
func (b *Builder) AddCount(start uint32, count uint64) error {
	if count == 0 {
		return fmt.Errorf("range at %s has no addresses", fromUint32(start))
	}
	last := uint64(start) + count - 1
	if last > 0xFFFFFFFF {
		return fmt.Errorf("range at %s with %d addresses runs past the end of the address space",
			fromUint32(start), count)
	}
	return b.AddRange(start, uint32(last))
}

// AddPrefix adds a CIDR block.
func (b *Builder) AddPrefix(p netip.Prefix) error {
	p = p.Masked()
	addr := p.Addr().Unmap()
	if !addr.Is4() {
		return fmt.Errorf("%s is not IPv4", p)
	}
	start := toUint32(addr.As4())
	size := uint64(1) << (32 - p.Bits())
	return b.AddCount(start, size)
}

// Build sorts and merges everything added so far. Adjacent ranges are joined,
// so the result is the smallest set of ranges covering the same addresses.
func (b *Builder) Build() *Set {
	if len(b.ranges) == 0 {
		return &Set{}
	}
	ranges := slices.Clone(b.ranges)
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].start != ranges[j].start {
			return ranges[i].start < ranges[j].start
		}
		return ranges[i].end < ranges[j].end
	})

	merged := make([]rng, 0, len(ranges))
	cur := ranges[0]
	for _, r := range ranges[1:] {
		// Join when overlapping or merely touching. The +1 is guarded so a
		// range ending at the last address cannot wrap around.
		if r.start <= cur.end || (cur.end != 0xFFFFFFFF && r.start == cur.end+1) {
			if r.end > cur.end {
				cur.end = r.end
			}
			continue
		}
		merged = append(merged, cur)
		cur = r
	}
	merged = append(merged, cur)

	s := &Set{
		starts: make([]uint32, len(merged)),
		ends:   make([]uint32, len(merged)),
	}
	for i, r := range merged {
		s.starts[i], s.ends[i] = r.start, r.end
	}
	return s
}

// Contains reports whether the address falls in the set. A nil or empty set
// contains nothing, which makes everything look foreign and therefore
// tunnelled: the safe direction to fail in.
func (s *Set) Contains(addr netip.Addr) bool {
	if s == nil || len(s.starts) == 0 {
		return false
	}
	addr = addr.Unmap()
	if !addr.Is4() {
		return false
	}
	v := toUint32(addr.As4())

	// The index of the last range that starts at or before v.
	i := sort.Search(len(s.starts), func(i int) bool { return s.starts[i] > v }) - 1
	return i >= 0 && v <= s.ends[i]
}

// Ranges returns how many ranges the set holds.
func (s *Set) Ranges() int {
	if s == nil {
		return 0
	}
	return len(s.starts)
}

// Addresses returns how many addresses the set covers.
func (s *Set) Addresses() uint64 {
	if s == nil {
		return 0
	}
	var total uint64
	for i := range s.starts {
		total += uint64(s.ends[i]) - uint64(s.starts[i]) + 1
	}
	return total
}

// ParseRIR reads a RIR "delegated extended" statistics file and keeps the
// IPv4 ranges delegated to the given two-letter country.
//
// Lines look like:
//
//	ripencc|RU|ipv4|2.60.0.0|524288|20120508|allocated|...
//
// Only allocated and assigned records count: available and reserved space is
// not in use by anyone.
func ParseRIR(r io.Reader, country string) (*Set, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if len(country) != 2 {
		return nil, fmt.Errorf("country must be two letters, got %q", country)
	}

	var b Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if text == "" || text[0] == '#' {
			continue
		}
		f := strings.Split(text, "|")
		if len(f) < 7 {
			continue // version and summary lines
		}
		if !strings.EqualFold(f[1], country) || f[2] != "ipv4" {
			continue
		}
		switch f[6] {
		case "allocated", "assigned":
		default:
			continue
		}

		addr, err := netip.ParseAddr(f[3])
		if err != nil || !addr.Unmap().Is4() {
			return nil, fmt.Errorf("line %d: %q is not an IPv4 address", line, f[3])
		}
		count, err := strconv.ParseUint(f[4], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("line %d: %q is not an address count", line, f[4])
		}
		if err := b.AddCount(toUint32(addr.Unmap().As4()), count); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	set := b.Build()
	if set.Ranges() == 0 {
		return nil, fmt.Errorf("no IPv4 ranges for %s in this file", country)
	}
	return set, nil
}

// WriteTo writes one inclusive range per line as "start-end". The form is
// chosen so the file can be read by eye and diffed between refreshes;
// delegations are not prefix aligned, so CIDR would not be lossless.
func (s *Set) WriteTo(w io.Writer) (int64, error) {
	bw := bufio.NewWriter(w)
	var n int64
	for i := range s.starts {
		c, err := fmt.Fprintf(bw, "%s-%s\n", fromUint32(s.starts[i]), fromUint32(s.ends[i]))
		n += int64(c)
		if err != nil {
			return n, err
		}
	}
	return n, bw.Flush()
}

// Read reads what WriteTo produced.
func Read(r io.Reader) (*Set, error) {
	var b Builder
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		first, last, ok := strings.Cut(text, "-")
		if !ok {
			return nil, fmt.Errorf("line %d: %q is not \"start-end\"", line, text)
		}
		start, err := netip.ParseAddr(strings.TrimSpace(first))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		end, err := netip.ParseAddr(strings.TrimSpace(last))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if !start.Unmap().Is4() || !end.Unmap().Is4() {
			return nil, fmt.Errorf("line %d: %q is not IPv4", line, text)
		}
		if err := b.AddRange(toUint32(start.Unmap().As4()), toUint32(end.Unmap().As4())); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return b.Build(), nil
}

func toUint32(b [4]byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func fromUint32(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
