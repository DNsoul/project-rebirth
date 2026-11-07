// Package enroll is how devices get onto a server without anyone registering
// them. A device proves it holds the content key, says who it is, and the
// server gives it an address and starts accepting its key.
//
// The server side is here and does not depend on the operating system: it is
// given somewhere to put peers (PeerSink) and a socket to listen on, so the
// whole of it can be tested in one process. The wire format is in package
// proto and spec/format.md section 4.5.
package enroll

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

	"awgrotator/internal/proto"
)

// DefaultRegistryPath is where a server keeps its devices.
const DefaultRegistryPath = "/var/lib/awgnode/registry.json"

// Limits of what one server holds.
const (
	DefaultMaxDevices = 32
	DefaultStaleAfter = 45 * 24 * time.Hour
)

// RegistryVersion is the layout of the file.
const RegistryVersion = 1

// Pool is the range of tunnel addresses handed to devices: the hosts From..To
// of a /24. The rest of the range belongs to the server and to the peers that
// are not devices (the health check, the enrollment peer).
type Pool struct {
	Subnet netip.Prefix
	From   int
	To     int
}

// NewPool checks and builds a pool.
func NewPool(subnet string, from, to int) (Pool, error) {
	p, err := netip.ParsePrefix(subnet)
	if err != nil {
		return Pool{}, fmt.Errorf("pool subnet %q: %w", subnet, err)
	}
	if !p.Addr().Is4() || p.Bits() != 24 {
		return Pool{}, fmt.Errorf("pool subnet %s: only an IPv4 /24 is supported", p)
	}
	if from < 1 || to > 254 || from > to {
		return Pool{}, fmt.Errorf("pool hosts %d..%d are not usable", from, to)
	}
	return Pool{Subnet: p.Masked(), From: from, To: to}, nil
}

func (p Pool) addr(host int) netip.Addr {
	b := p.Subnet.Addr().As4()
	b[3] = byte(host)
	return netip.AddrFrom4(b)
}

// Contains reports whether a is one of the addresses devices may have.
func (p Pool) Contains(a netip.Addr) bool {
	if !p.Subnet.Contains(a) {
		return false
	}
	h := int(a.As4()[3])
	return h >= p.From && h <= p.To
}

// Size is how many addresses the pool has.
func (p Pool) Size() int { return p.To - p.From + 1 }

// Device is one registered device.
type Device struct {
	// ID and PublicKey are base64url, as in the messages.
	ID        string `json:"id"`
	PublicKey string `json:"pub"`

	// Address is the device's address with prefix, e.g. "10.8.0.7/32".
	Address string `json:"address"`

	Name       string `json:"name,omitempty"`
	EnrolledAt int64  `json:"enrolled_at"`

	// LastSeen is when the device last completed a handshake, or when it
	// enrolled if it has not yet.
	LastSeen int64 `json:"last_seen"`
}

func (d Device) addr() (netip.Addr, error) {
	p, err := netip.ParsePrefix(d.Address)
	if err != nil {
		return netip.Addr{}, err
	}
	return p.Addr(), nil
}

type registryFile struct {
	Version int      `json:"version"`
	Devices []Device `json:"devices"`
}

// Registry is the list of devices a server accepts. It is kept in a file, so
// a restart does not lose anybody, and handed to the next server, so a move
// does not either.
type Registry struct {
	mu    sync.Mutex
	path  string
	pool  Pool
	max   int
	stale time.Duration

	devices []Device
	dirty   bool
}

// Options tune a registry. The zero value is usable.
type Options struct {
	// MaxDevices caps how many devices are held, at most the pool's size.
	MaxDevices int

	// StaleAfter is how long a device may go unseen before its place can be
	// given to a new one when the registry is full.
	StaleAfter time.Duration
}

func (o Options) withDefaults(p Pool) Options {
	if o.MaxDevices <= 0 {
		o.MaxDevices = DefaultMaxDevices
	}
	o.MaxDevices = min(o.MaxDevices, p.Size())
	if o.StaleAfter <= 0 {
		o.StaleAfter = DefaultStaleAfter
	}
	return o
}

// NewRegistry returns an empty registry that saves to path.
func NewRegistry(path string, pool Pool, opts Options) *Registry {
	opts = opts.withDefaults(pool)
	return &Registry{path: path, pool: pool, max: opts.MaxDevices, stale: opts.StaleAfter}
}

// LoadRegistry reads the registry from its file. A missing file is an empty
// registry. A file that cannot be understood is an error: the caller decides
// whether to start over, because dropping devices silently is not a decision a
// parser should make.
func LoadRegistry(path string, pool Pool, opts Options) (*Registry, error) {
	r := NewRegistry(path, pool, opts)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	devices, err := ParseRegistry(raw, pool)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r.devices = devices
	return r, nil
}

// ParseRegistry reads and checks the file's content. It is what a server uses
// on its own file and what a successor's manager can use to check what it is
// about to hand on.
func ParseRegistry(raw []byte, pool Pool) ([]Device, error) {
	var f registryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	if f.Version != RegistryVersion {
		return nil, fmt.Errorf("registry version %d, this build understands %d", f.Version, RegistryVersion)
	}
	ids, pubs, addrs := map[string]bool{}, map[string]bool{}, map[netip.Addr]bool{}
	for i, d := range f.Devices {
		if _, err := proto.Decode(d.ID, proto.DeviceIDSize); err != nil {
			return nil, fmt.Errorf("device %d id: %w", i, err)
		}
		if _, err := proto.Decode(d.PublicKey, proto.TunnelKeySize); err != nil {
			return nil, fmt.Errorf("device %d key: %w", i, err)
		}
		a, err := d.addr()
		if err != nil {
			return nil, fmt.Errorf("device %d address: %w", i, err)
		}
		if !pool.Contains(a) {
			return nil, fmt.Errorf("device %d address %s is outside the pool", i, a)
		}
		if ids[d.ID] || pubs[d.PublicKey] || addrs[a] {
			return nil, fmt.Errorf("device %d repeats an id, key or address", i)
		}
		ids[d.ID], pubs[d.PublicKey], addrs[a] = true, true, true
	}
	return f.Devices, nil
}

// Devices returns a copy of the list.
func (r *Registry) Devices() []Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Device(nil), r.devices...)
}

// Len is how many devices are registered.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.devices)
}

// Plan is a decided but not yet applied enrollment. Splitting the decision from
// the application lets the caller put the peer into the tunnel first and only
// then record it, so a failure part-way leaves nothing half done.
type Plan struct {
	Device Device

	// Existing is true when the device was already known.
	Existing bool

	// Drop lists keys to stop accepting: the device's previous key if it
	// changed, and the key of a stale device that makes room.
	Drop []string

	evicted string // id of the device removed to make room
}

// Plan decides what an enrollment does. A non-empty refusal is one of the
// proto.Enroll* reasons.
func (r *Registry) Plan(req *proto.EnrollRequest, now time.Time) (Plan, string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var existing *Device
	for i := range r.devices {
		d := &r.devices[i]
		if d.ID == req.DeviceID {
			existing = d
			continue
		}
		if d.PublicKey == req.PublicKey {
			return Plan{}, proto.EnrollKeyInUse
		}
	}

	if existing != nil {
		p := Plan{Device: *existing, Existing: true}
		p.Device.Name = req.Name
		p.Device.LastSeen = now.Unix()
		if existing.PublicKey != req.PublicKey {
			p.Drop = []string{existing.PublicKey}
			p.Device.PublicKey = req.PublicKey
		}
		return p, ""
	}

	p := Plan{Device: Device{
		ID: req.DeviceID, PublicKey: req.PublicKey, Name: req.Name,
		EnrolledAt: now.Unix(), LastSeen: now.Unix(),
	}}
	taken := map[netip.Addr]bool{}
	for _, d := range r.devices {
		if a, err := d.addr(); err == nil {
			taken[a] = true
		}
	}
	if len(r.devices) >= r.max {
		victim := r.staleVictim(now)
		if victim == nil {
			return Plan{}, proto.EnrollFull
		}
		p.Drop = append(p.Drop, victim.PublicKey)
		p.evicted = victim.ID
		if a, err := victim.addr(); err == nil {
			delete(taken, a)
		}
	}
	for h := r.pool.From; h <= r.pool.To; h++ {
		if a := r.pool.addr(h); !taken[a] {
			p.Device.Address = netip.PrefixFrom(a, 32).String()
			return p, ""
		}
	}
	return Plan{}, proto.EnrollFull
}

// staleVictim is the device that has gone unseen the longest, if that is long
// enough to give its place away.
func (r *Registry) staleVictim(now time.Time) *Device {
	var oldest *Device
	for i := range r.devices {
		d := &r.devices[i]
		if oldest == nil || d.LastSeen < oldest.LastSeen {
			oldest = d
		}
	}
	if oldest == nil || now.Sub(time.Unix(oldest.LastSeen, 0)) < r.stale {
		return nil
	}
	return oldest
}

// Commit applies a plan and saves. If saving fails nothing has changed.
func (r *Registry) Commit(p Plan) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	before := append([]Device(nil), r.devices...)
	next := make([]Device, 0, len(r.devices)+1)
	replaced := false
	for _, d := range r.devices {
		switch {
		case d.ID == p.evicted && p.evicted != "":
			continue
		case d.ID == p.Device.ID:
			next = append(next, p.Device)
			replaced = true
		default:
			next = append(next, d)
		}
	}
	if !replaced {
		next = append(next, p.Device)
	}
	sort.SliceStable(next, func(i, j int) bool {
		a, _ := next[i].addr()
		b, _ := next[j].addr()
		return a.Less(b)
	})
	r.devices = next

	if err := r.saveLocked(); err != nil {
		r.devices = before
		return err
	}
	return nil
}

// Touch records that the device with this key has just been seen. It only marks
// the registry as needing a save; Flush writes it.
func (r *Registry) Touch(publicKey string, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.devices {
		if r.devices[i].PublicKey == publicKey && t.Unix() > r.devices[i].LastSeen {
			r.devices[i].LastSeen = t.Unix()
			r.dirty = true
		}
	}
}

// Flush saves the registry if something changed since the last save.
func (r *Registry) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty {
		return nil
	}
	return r.saveLocked()
}

func (r *Registry) saveLocked() error {
	raw, err := json.MarshalIndent(registryFile{Version: RegistryVersion, Devices: r.devices}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	r.dirty = false
	return nil
}
