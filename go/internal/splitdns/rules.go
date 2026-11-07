// Package splitdns is the resolver that decides, per name, whether a site is
// reached directly or through the tunnel.
//
// The client never holds a list of foreign addresses. It sends every DNS
// query into the tunnel, and this resolver answers: either with the real
// addresses, which are outside the tunnel's routes so the connection goes out
// over the home connection, or with a stand-in address inside the tunnel's
// range that the server translates to the real one.
package splitdns

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/idna"
)

// Rules is a set of domain suffixes. A name matches when it, or any parent of
// it, is in the set: "mc.yandex.ru" matches "a.b.mc.yandex.ru" and
// "mc.yandex.ru" but not "yandex.ru".
type Rules struct {
	names map[string]struct{}
}

// NewRules builds a set from names, skipping invalid ones with an error.
func NewRules(names ...string) (*Rules, error) {
	r := &Rules{names: make(map[string]struct{}, len(names))}
	for _, n := range names {
		norm, err := normalizeName(n)
		if err != nil {
			return nil, err
		}
		r.names[norm] = struct{}{}
	}
	return r, nil
}

// ParseRules reads one name per line. '#' starts a comment, blank lines are
// ignored, and "*.example.com" means the same as "example.com".
func ParseRules(rd io.Reader) (*Rules, error) {
	r := &Rules{names: map[string]struct{}{}}
	sc := bufio.NewScanner(rd)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		name, err := normalizeName(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		r.names[name] = struct{}{}
	}
	return r, sc.Err()
}

// Match reports whether name, or a parent of it, is in the set. A nil set
// matches nothing.
func (r *Rules) Match(name string) bool {
	if r == nil || len(r.names) == 0 {
		return false
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for {
		if _, ok := r.names[name]; ok {
			return true
		}
		_, rest, ok := strings.Cut(name, ".")
		if !ok {
			return false
		}
		name = rest
	}
}

// Len returns how many names are in the set.
func (r *Rules) Len() int {
	if r == nil {
		return 0
	}
	return len(r.names)
}

// normalizeName lower-cases, strips a trailing dot and a "*." prefix, and
// converts internationalised names to the punycode form that appears on the
// wire. It rejects anything that could not be a domain name, because a rule
// that silently never matches is worse than one that fails to load.
func normalizeName(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")
	s = strings.TrimPrefix(s, "*.")
	s = strings.ToLower(s)
	if s == "" {
		return "", fmt.Errorf("empty domain name")
	}
	if strings.ContainsAny(s, " \t/\\:@*") {
		return "", fmt.Errorf("%q is not a domain name", s)
	}

	ascii, err := idna.Lookup.ToASCII(s)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid domain name: %w", s, err)
	}
	if len(ascii) > 253 {
		return "", fmt.Errorf("%q is too long for a domain name", s)
	}
	for _, label := range strings.Split(ascii, ".") {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("%q has an empty or over-long label", s)
		}
	}
	return ascii, nil
}
