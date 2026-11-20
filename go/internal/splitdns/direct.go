package splitdns

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/net/publicsuffix"
)

// The operator's own list of sites sent direct, kept apart from the rule
// files that arrive with a deployment. Those are edited on the operator's
// machine and replaced by every update; this one is edited from the bot while
// the server runs, lives with the server's other state and goes to its
// replacement.

// MaxDirect bounds the operator's list. It is chosen by hand, a site at a
// time; a list longer than this is a mistake, not a choice.
const MaxDirect = 500

// Reasons a name is refused.
var (
	ErrNotAName   = errors.New("not a domain name")
	ErrTooBroad   = errors.New("a public suffix, shared by unrelated sites")
	ErrDirectFull = errors.New("the list is full")
	ErrNotListed  = errors.New("not in the list")
)

// CheckDirectName normalises a name the operator wants sent direct, and
// refuses one that is not a site: a public suffix such as "com" or
// "cloudfront.net" would send thousands of unrelated sites past the tunnel,
// and the home address with them.
func CheckDirectName(s string) (string, error) {
	name, err := normalizeName(s)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotAName, err)
	}
	if !strings.Contains(name, ".") {
		return "", ErrTooBroad
	}
	if suffix, _ := publicsuffix.PublicSuffix(name); suffix == name {
		return "", ErrTooBroad
	}
	return name, nil
}

// DirectList is the operator's list, in the order the names were added, and
// the file it is kept in. Safe for concurrent use.
type DirectList struct {
	path string

	mu    sync.Mutex
	names []string
}

// OpenDirectList reads the list. A missing file is an empty list; one that
// cannot be read is an error, so it is not mistaken for an empty list and
// then saved over.
func OpenDirectList(path string) (*DirectList, error) {
	l := &DirectList{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if l.names, err = ParseDirectList(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

// ParseDirectList reads a list as written by Save: one name per line, '#'
// starting a comment. A name that no longer passes CheckDirectName is
// skipped rather than failing the list: the file is written only with names
// that passed, so such a name is one a newer list of public suffixes has
// since made too broad, and losing every other choice for it would be worse.
func ParseDirectList(raw []byte) ([]string, error) {
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		text := sc.Text()
		if i := strings.IndexByte(text, '#'); i >= 0 {
			text = text[:i]
		}
		if text = strings.TrimSpace(text); text == "" {
			continue
		}
		name, err := CheckDirectName(text)
		if err != nil {
			continue
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(names) > MaxDirect {
		return nil, fmt.Errorf("%d names: %w", len(names), ErrDirectFull)
	}
	return names, nil
}

// Names returns the list.
func (l *DirectList) Names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.names)
}

// Rules returns the list as a rule set.
func (l *DirectList) Rules() *Rules {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := &Rules{names: make(map[string]struct{}, len(l.names))}
	for _, n := range l.names {
		r.names[n] = struct{}{}
	}
	return r
}

// Add puts a name on the list and saves it. It returns the name as stored.
// A name already there is not an error.
func (l *DirectList) Add(s string) (string, error) {
	name, err := CheckDirectName(s)
	if err != nil {
		return "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if slices.Contains(l.names, name) {
		return name, nil
	}
	if len(l.names) >= MaxDirect {
		return "", ErrDirectFull
	}
	next := append(slices.Clone(l.names), name)
	if err := l.save(next); err != nil {
		return "", err
	}
	l.names = next
	return name, nil
}

// Remove takes a name off the list and saves it.
func (l *DirectList) Remove(s string) error {
	name, err := normalizeName(s)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotAName, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	i := slices.Index(l.names, name)
	if i < 0 {
		return ErrNotListed
	}
	next := slices.Delete(slices.Clone(l.names), i, i+1)
	if err := l.save(next); err != nil {
		return err
	}
	l.names = next
	return nil
}

func (l *DirectList) save(names []string) error {
	var b strings.Builder
	b.WriteString("# Сайты, которые владелец пустил напрямую из бота. Поддомены входят сами.\n")
	for _, n := range names {
		b.WriteString(n + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}
