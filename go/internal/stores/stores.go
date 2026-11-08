// Package stores keeps the signed files of the configuration in more than one
// place, with no place in charge of the others.
//
// The same five files live in a secret gist on GitHub and in a package on
// GitLab. A file is written to every place that will take it. A place that
// could not take it is simply behind, and Reconcile, which is run now and then,
// copies the newest verified version of each file over to whatever is behind.
// Reading goes the same way in reverse: every place is asked, and of each file
// the newest version that verifies wins.
//
// None of this needs the places to be trusted. Every file is encrypted and
// signed, and what "newer" means is decided from what is inside it, never from
// the place it was found, so a place can be stale or even hostile and only
// cost time.
package stores

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"awgrotator/internal/proto"
)

// Names are the files kept in every place, in the order they are listed.
var Names = []string{
	proto.FileConfig,
	proto.FileStatus,
	proto.FileCommand,
	proto.FileRevocation,
	proto.FileRelease,
}

// Store is one place the files can be kept.
type Store interface {
	// Name is how the place is called in messages.
	Name() string

	// Read returns the named files that exist; one that is not there is simply
	// missing from the result. An error means the place could not be asked.
	Read(ctx context.Context, names ...string) (map[string]string, error)

	// Write puts the files there, replacing what was there under those names.
	Write(ctx context.Context, files map[string]string) error
}

// Set is a group of places that mirror each other.
type Set struct {
	Stores []Store

	// Trust is what a file is verified with to tell which version is newer.
	Trust proto.Trust

	// Now is the clock; tests replace it.
	Now func() time.Time
}

func (s *Set) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// view is what one place answered.
type view struct {
	store Store
	files map[string]string
	err   error
}

func (s *Set) readAll(ctx context.Context, names []string) []view {
	views := make([]view, len(s.Stores))
	var wg sync.WaitGroup
	for i, st := range s.Stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			files, err := st.Read(ctx, names...)
			views[i] = view{store: st, files: files, err: err}
		}()
	}
	wg.Wait()
	return views
}

// Read asks every place and returns, for each file, the newest version that
// verifies. A file that none of them has is missing from the result. It fails
// only when no place could be asked at all.
func (s *Set) Read(ctx context.Context, names ...string) (map[string]string, error) {
	if len(names) == 0 {
		names = Names
	}
	views := s.readAll(ctx, names)

	var errs []error
	for _, v := range views {
		if v.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", v.store.Name(), v.err))
		}
	}
	if len(errs) == len(views) {
		return nil, fmt.Errorf("no place could be read: %w", errors.Join(errs...))
	}

	now := s.now()
	out := make(map[string]string, len(names))
	for _, name := range names {
		if p := s.best(name, views, now); p.found {
			out[name] = p.blob
		}
	}
	return out, nil
}

// Outcome is what one place did with a write.
type Outcome struct {
	Store string
	Err   error
}

// Result says, place by place, whether a write was taken.
type Result struct{ Outcomes []Outcome }

// Accepted counts the places that took the write.
func (r Result) Accepted() int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Err == nil {
			n++
		}
	}
	return n
}

// Failed lists the places that did not.
func (r Result) Failed() []Outcome {
	var out []Outcome
	for _, o := range r.Outcomes {
		if o.Err != nil {
			out = append(out, o)
		}
	}
	return out
}

// String is the failures, for a log line; empty when nothing failed.
func (r Result) String() string {
	var parts []string
	for _, o := range r.Failed() {
		parts = append(parts, fmt.Sprintf("%s: %v", o.Store, o.Err))
	}
	return strings.Join(parts, "; ")
}

// Write puts the files in every place at once. It succeeds when at least one
// place took them; the ones that did not are in the result, and are caught up
// later by Reconcile.
func (s *Set) Write(ctx context.Context, files map[string]string) (Result, error) {
	res := Result{Outcomes: make([]Outcome, len(s.Stores))}
	var wg sync.WaitGroup
	for i, st := range s.Stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res.Outcomes[i] = Outcome{Store: st.Name(), Err: st.Write(ctx, files)}
		}()
	}
	wg.Wait()

	if res.Accepted() == 0 {
		var errs []error
		for _, o := range res.Outcomes {
			errs = append(errs, fmt.Errorf("%s: %w", o.Store, o.Err))
		}
		return res, fmt.Errorf("no place took the write: %w", errors.Join(errs...))
	}
	return res, nil
}

// Report is what a Reconcile found and did.
type Report struct {
	// Missed are the places that could not be read, with the reason.
	Missed map[string]error

	// Copied are the files put into each place that was behind.
	Copied map[string][]string

	// Failed are the places a copy could not be written to.
	Failed map[string]error
}

// Synced reports that every place was reachable and now holds the newest of
// everything.
func (r Report) Synced() bool { return len(r.Missed) == 0 && len(r.Failed) == 0 }

// String is a line for a log.
func (r Report) String() string {
	var parts []string
	for _, name := range sortedKeys(r.Missed) {
		parts = append(parts, fmt.Sprintf("%s unreachable (%v)", name, r.Missed[name]))
	}
	for _, name := range sortedKeys(r.Copied) {
		parts = append(parts, fmt.Sprintf("%s caught up: %s", name, strings.Join(r.Copied[name], ", ")))
	}
	for _, name := range sortedKeys(r.Failed) {
		parts = append(parts, fmt.Sprintf("%s still behind (%v)", name, r.Failed[name]))
	}
	if len(parts) == 0 {
		return "all places agree"
	}
	return strings.Join(parts, "; ")
}

// Reconcile makes the places agree: of each file it takes the newest version
// that verifies and writes it to every place that has an older one, a broken
// one, or none. It never writes a version that is not newer than the one it
// replaces, so a server that has been replaced cannot undo its successor.
//
// It fails only when no place could be read. A place that could not be read or
// written is in the report, to be tried again next time.
func (s *Set) Reconcile(ctx context.Context, names ...string) (Report, error) {
	if len(names) == 0 {
		names = Names
	}
	views := s.readAll(ctx, names)
	rep := Report{Missed: map[string]error{}, Copied: map[string][]string{}, Failed: map[string]error{}}

	reached := 0
	for _, v := range views {
		if v.err != nil {
			rep.Missed[v.store.Name()] = v.err
		} else {
			reached++
		}
	}
	if reached == 0 {
		return rep, errors.New("no place could be read")
	}

	now := s.now()
	lacks := make([]map[string]string, len(views))
	for _, name := range names {
		p := s.best(name, views, now)
		if !p.found || !p.valid {
			continue
		}
		for i, v := range views {
			if v.err != nil {
				continue
			}
			behind := true
			if blob := v.files[name]; blob != "" {
				if r, ok := s.rank(name, blob, now); ok {
					behind = p.rank.after(r)
				}
			}
			if behind {
				if lacks[i] == nil {
					lacks[i] = map[string]string{}
				}
				lacks[i][name] = p.blob
			}
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, files := range lacks {
		if len(files) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := views[i].store.Write(ctx, files)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				rep.Failed[views[i].store.Name()] = err
				return
			}
			rep.Copied[views[i].store.Name()] = sortedKeys(files)
		}()
	}
	wg.Wait()
	return rep, nil
}

// pick is the newest version of one file found among the places.
type pick struct {
	blob  string
	rank  rank
	valid bool // it verified; if not, it is only the first one found
	found bool
}

// best chooses among what the places hold. A version that verifies beats one
// that does not, and of two that verify the newer wins; a tie goes to the
// place listed first.
func (s *Set) best(name string, views []view, now time.Time) pick {
	var p pick
	for _, v := range views {
		if v.err != nil {
			continue
		}
		blob := v.files[name]
		if blob == "" {
			continue
		}
		r, ok := s.rank(name, blob, now)
		switch {
		case !p.found:
			p = pick{blob: blob, rank: r, valid: ok, found: true}
		case ok && (!p.valid || r.after(p.rank)):
			p = pick{blob: blob, rank: r, valid: true, found: true}
		}
	}
	return p
}

// rank says how new a version of a file is. Two numbers, compared in order:
// what the file kind counts by, then when it was issued.
type rank struct{ major, minor uint64 }

func (a rank) after(b rank) bool {
	return a.major > b.major || (a.major == b.major && a.minor > b.minor)
}

// rank verifies the file the way an app would and says how new it is. ok is
// false for anything that does not verify: a placeholder, a forgery, a
// document that has expired.
func (s *Set) rank(name, blob string, now time.Time) (rank, bool) {
	switch name {
	case proto.FileConfig:
		d, _, err := s.Trust.OpenDoc(blob, proto.KindConfig, now, proto.State{})
		if err != nil {
			return rank{}, false
		}
		return rank{d.Seq, uint64(d.IssuedAt)}, true
	case proto.FileStatus:
		d, _, err := s.Trust.OpenDoc(blob, proto.KindStatus, now, proto.State{})
		if err != nil {
			return rank{}, false
		}
		return rank{d.Seq, uint64(d.IssuedAt)}, true
	case proto.FileRevocation:
		r, _, err := s.Trust.OpenRevocation(blob, now, proto.State{})
		if err != nil {
			return rank{}, false
		}
		return rank{uint64(r.MinEpoch), uint64(r.IssuedAt)}, true
	case proto.FileRelease:
		r, _, err := s.Trust.OpenRelease(blob, proto.State{})
		if err != nil {
			return rank{}, false
		}
		return rank{uint64(r.Build), uint64(r.IssuedAt)}, true
	case proto.FileCommand:
		c, err := s.Trust.OpenCommand(blob, now)
		if err != nil {
			return rank{}, false
		}
		return rank{uint64(c.IssuedAt), 0}, true
	}
	return rank{}, false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
