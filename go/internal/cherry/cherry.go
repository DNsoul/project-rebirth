// Package cherry is the slice of the Cherry Servers API this project needs:
// find the team, keep a project of our own, register an SSH key, and create,
// inspect, list and delete servers.
//
// A Cherry key reaches the whole account and cannot be narrowed, so this
// package never lists servers across the account: only inside one project.
// What counts as ours is decided one level up, in the cloud adapter.
package cherry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const api = "https://api.cherryservers.com/v1"

// ErrNotFound is returned when the server asked about does not exist.
var ErrNotFound = errors.New("not found")

// Client talks to the Cherry Servers API.
type Client struct {
	key  string
	base string
	http *http.Client
}

// New returns a client using the given API key.
func New(key string) *Client {
	return &Client{
		key:  key,
		base: api,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// WithBase points the client at another address, such as a test server.
func (c *Client) WithBase(base string) *Client {
	c.base = base
	return c
}

// Team is the account's team; projects live inside it.
//
// The API also returns the account holder's billing details (name, address)
// with a team. They are deliberately not decoded: nothing here needs them, and
// what is not read cannot end up in a log or a message.
type Team struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Credit Credit `json:"credit"`
}

// Credit is the money on the team's balance.
type Credit struct {
	// Account is what was paid in; Promo is what Cherry gave for free.
	Account Money `json:"account"`
	Promo   Money `json:"promo"`
}

// Money is an amount left on a balance.
type Money struct {
	Remaining float64 `json:"remaining"`
	Currency  string  `json:"currency"`
}

// Project groups servers.
type Project struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Server is one server as the API describes it.
type Server struct {
	ID       int               `json:"id"`
	Hostname string            `json:"hostname"`
	State    string            `json:"state"`
	Status   string            `json:"status"`
	Region   Slug              `json:"region"`
	Plan     Plan              `json:"plan"`
	Project  Project           `json:"project"`
	IPs      []IPAddress       `json:"ip_addresses"`
	Tags     map[string]string `json:"tags"`
	Created  string            `json:"created_at"`

	// TerminationDate is set for a server on a longer billing cycle: the day
	// the paid period ends.
	TerminationDate string `json:"termination_date"`

	// Pricing is what the server costs and when it is paid for next.
	Pricing *Pricing `json:"pricing"`

	// TrafficUsed is the outgoing traffic counted against the allowance. Cherry
	// counts only what leaves its servers, over a rolling thirty days.
	TrafficUsed int64 `json:"traffic_used_bytes"`
}

// Slug names a region.
type Slug struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Plan is a server's size, with what it includes.
type Plan struct {
	Slug
	Specs struct {
		Bandwidth struct {
			// Name is the monthly traffic allowance as Cherry writes it: "1TB".
			Name string `json:"name"`
		} `json:"bandwidth"`
	} `json:"specs"`
}

// Pricing is a server's price and its next payment.
type Pricing struct {
	Currency string `json:"currency"`

	// Unit is the billing period, "Hourly" or "Monthly".
	Unit string `json:"unit"`

	// Price is per period with tax included; PriceTotal says the same in the
	// samples seen, and is preferred when given.
	Price      float64 `json:"price"`
	PriceTotal float64 `json:"price_total"`

	// NextPaymentAt is when the paid period ends and the next one is due, as
	// RFC 3339 with the account's offset. Empty for a server paid by the hour.
	NextPaymentAt string `json:"next_payment_at"`

	// AutoRenewal says Cherry pays the next period itself. Without a card on
	// the account it is off, and the server stops at NextPaymentAt unless the
	// period is paid by hand from the team's balance.
	AutoRenewal bool `json:"auto_renewal"`
}

// IPAddress is one address of a server.
type IPAddress struct {
	Address       string `json:"address"`
	AddressFamily int    `json:"address_family"`
	Type          string `json:"type"`
}

// PublicIPv4 returns the server's public IPv4 address, or "".
func (s Server) PublicIPv4() string { return s.public(4) }

// PublicIPv6 returns the server's public IPv6 address, or "".
func (s Server) PublicIPv6() string { return s.public(6) }

func (s Server) public(family int) string {
	var fallback string
	for _, ip := range s.IPs {
		if ip.AddressFamily != family || ip.Type == "private-ip" || ip.Address == "" {
			continue
		}
		if ip.Type == "primary-ip" {
			return ip.Address
		}
		if fallback == "" {
			fallback = ip.Address
		}
	}
	return fallback
}

// Ready reports whether the server is installed and has an address to connect to.
func (s Server) Ready() bool {
	return s.Status == "deployed" && s.PublicIPv4() != ""
}

// SSHKey is a public key registered with the account.
type SSHKey struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
	Key   string `json:"key"`
}

// CreateRequest describes a new server.
type CreateRequest struct {
	Plan          string            `json:"plan"`
	Region        string            `json:"region"`
	Image         string            `json:"image"`
	Hostname      string            `json:"hostname,omitempty"`
	SSHKeys       []string          `json:"ssh_keys,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	Cycle         string            `json:"cycle,omitempty"`
	ConfigureIPv6 bool              `json:"configure_ipv6"`
}

// Teams lists the teams the key belongs to.
func (c *Client) Teams(ctx context.Context) ([]Team, error) {
	var out []Team
	return out, c.do(ctx, http.MethodGet, "/teams", nil, &out)
}

// Projects lists a team's projects.
func (c *Client) Projects(ctx context.Context, team int) ([]Project, error) {
	var out []Project
	return out, c.do(ctx, http.MethodGet, fmt.Sprintf("/teams/%d/projects", team), nil, &out)
}

// CreateProject makes a new project in a team.
func (c *Client) CreateProject(ctx context.Context, team int, name string) (*Project, error) {
	in := struct {
		Name string `json:"name"`
	}{name}
	var out Project
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/teams/%d/projects", team), in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Servers lists the servers in one project, and only there.
func (c *Client) Servers(ctx context.Context, project int) ([]Server, error) {
	var out []Server
	path := fmt.Sprintf("/projects/%d/servers?limit=500", project)
	return out, c.do(ctx, http.MethodGet, path, nil, &out)
}

// Server fetches one server. A server that does not exist is ErrNotFound.
func (c *Client) Server(ctx context.Context, id int) (*Server, error) {
	var out Server
	if err := c.do(ctx, http.MethodGet, "/servers/"+strconv.Itoa(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateServer orders a new server in a project.
func (c *Client) CreateServer(ctx context.Context, project int, req CreateRequest) (*Server, error) {
	var out Server
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%d/servers", project), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteServer destroys a server.
func (c *Client) DeleteServer(ctx context.Context, id int) error {
	return c.do(ctx, http.MethodDelete, "/servers/"+strconv.Itoa(id), nil, nil)
}

// SSHKeys lists the registered public keys.
func (c *Client) SSHKeys(ctx context.Context) ([]SSHKey, error) {
	var out []SSHKey
	return out, c.do(ctx, http.MethodGet, "/ssh-keys", nil, &out)
}

// CreateSSHKey registers a public key and returns its id.
func (c *Client) CreateSSHKey(ctx context.Context, label, pub string) (int, error) {
	in := struct {
		Label string `json:"label"`
		Key   string `json:"key"`
	}{label, pub}
	var out SSHKey
	if err := c.do(ctx, http.MethodPost, "/ssh-keys", in, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// Error is an answer from the API that is not a success.
type Error struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *Error) Error() string {
	return fmt.Sprintf("cherry %s %s: %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// Is makes a 404 match ErrNotFound.
func (e *Error) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The URL in a transport error carries nothing secret, but say only
		// what failed, the same way for every request.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("cherry %s %s: %w", method, path, ue.Err)
		}
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Method: method, Path: strings.SplitN(path, "?", 2)[0], Status: resp.StatusCode, Message: errorMessage(raw)}
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("cherry response is not valid JSON: %w", err)
		}
	}
	return nil
}

func errorMessage(b []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return e.Message
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
