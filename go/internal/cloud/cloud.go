// Package cloud is what the rest of the project knows about hosting providers:
// a server, where it is, and the handful of things done with it. Each provider
// is one adapter in this package over its own API client; the engine, the
// deploy code and the operator's tool talk only to the interface here.
//
// Adding a provider is: an API client package, an adapter file here that
// registers itself, and its cities in the manager's list of places.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Billing is how a server is paid for.
type Billing string

const (
	// Hourly servers are paid for the time they run. Automatic moves go only
	// to places billed this way.
	Hourly Billing = "hourly"

	// Monthly servers are paid a month in advance. They are only ever chosen
	// by the operator, and are left before the next month is charged.
	Monthly Billing = "monthly"
)

// Valid reports whether b is a known billing.
func (b Billing) Valid() bool { return b == Hourly || b == Monthly }

// PaidPeriod is how long a monthly server is paid for. Providers bill a month
// as thirty days or a calendar month; thirty days is the shorter of the two.
const PaidPeriod = 30 * 24 * time.Hour

// Errors the engine acts on.
var (
	// ErrNoCapacity means the provider has no server of this kind free in that
	// city right now.
	ErrNoCapacity = errors.New("no servers of this kind are free there")

	// ErrNotFound means there is no such server.
	ErrNotFound = errors.New("no such server")

	// ErrNotOurs means the server exists but was not made by this project, so
	// it is not touched.
	ErrNotOurs = errors.New("the server is not one of ours")
)

// Server is one server, whoever hosts it.
type Server struct {
	Provider string
	ID       string

	// Location is our own code for the city (sto, ams, sia…), the same for
	// every provider, and what the apps show.
	Location string

	IP      string
	IPv6    string
	Label   string
	Created time.Time

	// Ready means installed, running and with an address to connect to.
	Ready bool
}

// Ref names the server across providers, for logs and messages.
func (s Server) Ref() string { return s.Provider + "/" + s.ID }

// Spec describes a server to create.
type Spec struct {
	Location string
	Label    string
	SSHKeyID string
	Billing  Billing
}

// Provider is one hosting provider, seen through the project's own tag: List
// and Delete never reach a server this project did not create.
type Provider interface {
	Name() string

	// Locations are the cities this provider can be asked for.
	Locations() []string

	Create(ctx context.Context, spec Spec) (*Server, error)

	// Get fetches one of our servers; ErrNotFound when it does not exist,
	// ErrNotOurs when it is someone else's.
	Get(ctx context.Context, id string) (*Server, error)

	// List returns our servers, and only ours.
	List(ctx context.Context) ([]Server, error)

	// Delete destroys one of our servers. A server that is already gone
	// counts as deleted; one that is not ours is refused with ErrNotOurs.
	Delete(ctx context.Context, id string) error

	// EnsureSSHKey registers a public key once and returns its id.
	EnsureSSHKey(ctx context.Context, label, pub string) (string, error)
}

// Settings is how one provider is used: the credentials and what servers are
// made of. Fields a provider does not need are left empty.
type Settings struct {
	Key string `json:"key"`

	// Plan and Image are the provider's own names for the size and the
	// operating system (for Vultr, Image is the OS id).
	Plan  string `json:"plan"`
	Image string `json:"image"`

	// Tag marks every server this project creates.
	Tag string `json:"tag"`

	// Project is where the servers live, for providers that have projects.
	Project int `json:"project_id,omitempty"`

	// SSHKeyID is our public key's registration with the provider.
	SSHKeyID string `json:"ssh_key_id"`
}

type opener func(Settings) (Provider, error)

var registry = map[string]opener{}

func register(name string, open opener) { registry[name] = open }

// Open returns the named provider configured with s.
func Open(name string, s Settings) (Provider, error) {
	open, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown hosting provider %q (known: %s)", name, strings.Join(Names(), ", "))
	}
	if s.Key == "" {
		return nil, fmt.Errorf("%s: no API key", name)
	}
	return open(s)
}

// Names lists the known providers.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Place is a provider and a city: where a server can be put.
type Place struct {
	Provider string
	Location string
}

// ParsePlace reads "provider:city". A bare city is taken as Vultr's, which is
// how places were written before there was a second provider.
func ParsePlace(s string) (Place, error) {
	s = strings.TrimSpace(s)
	provider, location, ok := strings.Cut(s, ":")
	if !ok {
		provider, location = "vultr", s
	}
	if provider == "" || location == "" || strings.ContainsAny(location, ": ") {
		return Place{}, fmt.Errorf("%q is not a place: write it as provider:city, for example cherry:ams", s)
	}
	return Place{Provider: provider, Location: location}, nil
}

func (p Place) String() string { return p.Provider + ":" + p.Location }

// WaitReady polls until the server is ready, or the context runs out.
func WaitReady(ctx context.Context, p Provider, id string) (*Server, error) {
	return waitReady(ctx, p, id, 5*time.Second)
}

func waitReady(ctx context.Context, p Provider, id string, every time.Duration) (*Server, error) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		s, err := p.Get(ctx, id)
		if err == nil && s.Ready {
			return s, nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return nil, fmt.Errorf("waiting for server %s/%s: %w", p.Name(), id, err)
			}
			return nil, fmt.Errorf("server %s/%s did not come up in time", p.Name(), id)
		case <-ticker.C:
		}
	}
}

// Set is every provider in use, by name.
type Set map[string]Provider

// Get returns the named provider.
func (s Set) Get(name string) (Provider, error) {
	p, ok := s[name]
	if !ok {
		return nil, fmt.Errorf("hosting provider %q is not set up here", name)
	}
	return p, nil
}

// List returns our servers at every provider. It fails if any provider
// cannot be asked: a count that silently misses one would let the fleet grow
// past its limit.
func (s Set) List(ctx context.Context) ([]Server, error) {
	var all []Server
	for _, name := range s.names() {
		servers, err := s[name].List(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		all = append(all, servers...)
	}
	return all, nil
}

// Delete destroys one of our servers at the named provider.
func (s Set) Delete(ctx context.Context, provider, id string) error {
	p, err := s.Get(provider)
	if err != nil {
		return err
	}
	return p.Delete(ctx, id)
}

func (s Set) names() []string {
	out := make([]string, 0, len(s))
	for n := range s {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// capacityWords are what providers say when a size is sold out in a city.
var capacityWords = []string{"out of stock", "not available", "no available", "unavailable", "capacity", "sold out"}

func looksLikeNoCapacity(msg string) bool {
	m := strings.ToLower(msg)
	for _, w := range capacityWords {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}
