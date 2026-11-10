package enroll

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"awgrotator/internal/proto"
)

// DefaultAccessPath is where a server keeps the requests waiting for the
// operator and the devices the operator refused. The approved devices stay in
// the registry, whose layout this does not touch, so a program that knows
// nothing of approval reads the registry exactly as before.
const DefaultAccessPath = "/var/lib/awgnode/access.json"

// AccessVersion is the layout of the file.
const AccessVersion = 1

// Limits of the waiting list. The app is public, so anyone can ask; a cap and
// an age limit keep a stranger from filling the list or the operator's chat.
const (
	DefaultMaxPending = 20
	DefaultPendingFor = 7 * 24 * time.Hour
)

// ErrQueueFull is returned when the waiting list holds as many requests as it
// will.
var ErrQueueFull = errors.New("the waiting list is full")

// Waiting is a device waiting for the operator's decision.
type Waiting struct {
	// ID and PublicKey are base64url, as in the messages.
	ID        string `json:"id"`
	PublicKey string `json:"pub"`
	Name      string `json:"name,omitempty"`

	// AskedAt is when the device first asked, LastAsked when it last did.
	AskedAt   int64 `json:"asked_at"`
	LastAsked int64 `json:"last_asked"`

	// Notified says the operator has been told about this request, so a restart
	// or a move does not tell them again.
	Notified bool `json:"notified,omitempty"`

	// KeyChange says the device is already approved under another key: an app
	// that was reinstalled, or somebody claiming to be that device. Approving
	// it moves the device to the new key and keeps its address.
	KeyChange bool `json:"key_change,omitempty"`
}

// Denial is a device, or a single key of an approved device, that the operator
// refused or cut off.
type Denial struct {
	ID        string `json:"id"`
	PublicKey string `json:"pub"`
	Name      string `json:"name,omitempty"`
	At        int64  `json:"at"`

	// KeyOnly limits the refusal to this key: it was a key change of a device
	// that stays approved under its old key.
	KeyOnly bool `json:"key_only,omitempty"`
}

type accessFile struct {
	Version int       `json:"version"`
	Pending []Waiting `json:"pending"`
	Denied  []Denial  `json:"denied"`
}

// Access is the waiting list and the refusals. It is kept in a file next to the
// registry and handed to the next server together with it.
type Access struct {
	mu         sync.Mutex
	path       string
	maxPending int
	pendingFor time.Duration

	pending []Waiting
	denied  []Denial
}

// AccessOptions tune the waiting list. The zero value is usable.
type AccessOptions struct {
	MaxPending int
	PendingFor time.Duration
}

// NewAccess returns an empty list that saves to path.
func NewAccess(path string, opts AccessOptions) *Access {
	if opts.MaxPending <= 0 {
		opts.MaxPending = DefaultMaxPending
	}
	if opts.PendingFor <= 0 {
		opts.PendingFor = DefaultPendingFor
	}
	return &Access{path: path, maxPending: opts.MaxPending, pendingFor: opts.PendingFor}
}

// LoadAccess reads the list from its file. A missing file is an empty list; a
// file that cannot be understood is an error, as with the registry.
func LoadAccess(path string, opts AccessOptions) (*Access, error) {
	a := NewAccess(path, opts)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	pending, denied, err := ParseAccess(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	a.pending, a.denied = pending, denied
	return a, nil
}

// ParseAccess reads and checks the file's content. It is what a server uses on
// its own file and what a manager uses to check what it is about to hand on.
func ParseAccess(raw []byte) ([]Waiting, []Denial, error) {
	f, err := parseAccess(raw)
	if err != nil {
		return nil, nil, err
	}
	return f.Pending, f.Denied, nil
}

func parseAccess(raw []byte) (accessFile, error) {
	var f accessFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return accessFile{}, fmt.Errorf("not valid JSON: %w", err)
	}
	if f.Version != AccessVersion {
		return accessFile{}, fmt.Errorf("access list version %d, this build understands %d", f.Version, AccessVersion)
	}
	check := func(what string, i int, id, pub string) error {
		if _, err := proto.Decode(id, proto.DeviceIDSize); err != nil {
			return fmt.Errorf("%s %d id: %w", what, i, err)
		}
		if _, err := proto.Decode(pub, proto.TunnelKeySize); err != nil {
			return fmt.Errorf("%s %d key: %w", what, i, err)
		}
		return nil
	}
	seen := map[string]bool{}
	for i, r := range f.Pending {
		if err := check("request", i, r.ID, r.PublicKey); err != nil {
			return accessFile{}, err
		}
		if seen[r.ID] {
			return accessFile{}, fmt.Errorf("request %d repeats an id", i)
		}
		seen[r.ID] = true
	}
	seen = map[string]bool{}
	for i, d := range f.Denied {
		if err := check("refusal", i, d.ID, d.PublicKey); err != nil {
			return accessFile{}, err
		}
		if seen[d.ID] {
			return accessFile{}, fmt.Errorf("refusal %d repeats an id", i)
		}
		seen[d.ID] = true
	}
	return f, nil
}

// Pending returns a copy of the waiting list, without the requests that have
// waited too long.
func (a *Access) Pending(now time.Time) []Waiting {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	return append([]Waiting(nil), a.pending...)
}

// Denied returns a copy of the refusals.
func (a *Access) Denied() []Denial {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Denial(nil), a.denied...)
}

// Refused reports whether this device, or this key of it, has been refused. A
// refused device that comes back with a new key (the app reinstalled) has its
// refusal brought up to date, so that if the operator changes their mind, the
// key they let in is the one the device is using now.
func (a *Access) Refused(req *proto.EnrollRequest) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.denied {
		d := &a.denied[i]
		if d.KeyOnly {
			if d.PublicKey == req.PublicKey {
				return true
			}
			continue
		}
		if d.ID != req.DeviceID {
			continue
		}
		if d.PublicKey != req.PublicKey || d.Name != req.Name {
			before := *d
			d.PublicKey, d.Name = req.PublicKey, req.Name
			if err := a.saveLocked(); err != nil {
				*d = before
			}
		}
		return true
	}
	return false
}

// Ask records a request, or refreshes the one the device already has. It
// reports whether the operator still has to be told: a new request, or one
// whose key changed since they were told, because the code they would match
// against has changed with it.
func (a *Access) Ask(req *proto.EnrollRequest, keyChange bool, now time.Time) (Waiting, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)

	for i := range a.pending {
		r := &a.pending[i]
		if r.ID != req.DeviceID {
			continue
		}
		r.LastAsked = now.Unix()
		if r.PublicKey == req.PublicKey && r.Name == req.Name && r.KeyChange == keyChange {
			// Only the time moved on. Not worth a disk write on every poll; it
			// is saved with the next real change.
			return *r, !r.Notified, nil
		}
		before := *r
		if r.PublicKey != req.PublicKey {
			r.Notified = false
		}
		r.PublicKey, r.Name, r.KeyChange = req.PublicKey, req.Name, keyChange
		if err := a.saveLocked(); err != nil {
			*r = before
			return Waiting{}, false, err
		}
		return *r, !r.Notified, nil
	}

	if len(a.pending) >= a.maxPending {
		return Waiting{}, false, ErrQueueFull
	}
	r := Waiting{
		ID: req.DeviceID, PublicKey: req.PublicKey, Name: req.Name,
		AskedAt: now.Unix(), LastAsked: now.Unix(), KeyChange: keyChange,
	}
	a.pending = append(a.pending, r)
	if err := a.saveLocked(); err != nil {
		a.pending = a.pending[:len(a.pending)-1]
		return Waiting{}, false, err
	}
	return r, true, nil
}

// Request returns the waiting request of a device.
func (a *Access) Request(id string) (Waiting, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.pending {
		if r.ID == id {
			return r, true
		}
	}
	return Waiting{}, false
}

// Denial returns the refusal of a device.
func (a *Access) Denial(id string) (Denial, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, d := range a.denied {
		if d.ID == id {
			return d, true
		}
	}
	return Denial{}, false
}

// Settle removes a device's request and, with deny set, records the refusal in
// the same save. A device has at most one of each, so a new refusal replaces an
// old one.
func (a *Access) Settle(id string, deny *Denial) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	beforeP := append([]Waiting(nil), a.pending...)
	beforeD := append([]Denial(nil), a.denied...)

	a.pending = dropRequest(a.pending, id)
	if deny != nil {
		a.denied = append(dropDenial(a.denied, deny.ID), *deny)
	}
	if err := a.saveLocked(); err != nil {
		a.pending, a.denied = beforeP, beforeD
		return err
	}
	return nil
}

// Forget removes a device's refusal, so it can ask again.
func (a *Access) Forget(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	before := append([]Denial(nil), a.denied...)
	a.denied = dropDenial(a.denied, id)
	if err := a.saveLocked(); err != nil {
		a.denied = before
		return err
	}
	return nil
}

// MarkNotified records that the operator has been told about these requests.
func (a *Access) MarkNotified(ids []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	changed := false
	for i := range a.pending {
		if want[a.pending[i].ID] && !a.pending[i].Notified {
			a.pending[i].Notified = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return a.saveLocked()
}

func (a *Access) expireLocked(now time.Time) {
	kept := a.pending[:0]
	for _, r := range a.pending {
		if now.Sub(time.Unix(r.LastAsked, 0)) < a.pendingFor {
			kept = append(kept, r)
		}
	}
	a.pending = kept
}

func dropRequest(list []Waiting, id string) []Waiting {
	out := make([]Waiting, 0, len(list))
	for _, r := range list {
		if r.ID != id {
			out = append(out, r)
		}
	}
	return out
}

func dropDenial(list []Denial, id string) []Denial {
	out := make([]Denial, 0, len(list))
	for _, d := range list {
		if d.ID != id {
			out = append(out, d)
		}
	}
	return out
}

func (a *Access) saveLocked() error {
	pending, denied := a.pending, a.denied
	if pending == nil {
		pending = []Waiting{}
	}
	if denied == nil {
		denied = []Denial{}
	}
	raw, err := json.MarshalIndent(accessFile{Version: AccessVersion, Pending: pending, Denied: denied}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}
