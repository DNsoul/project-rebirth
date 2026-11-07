// Package vultr is the slice of the Vultr API this project needs: list
// regions, register an SSH key, and create, inspect and delete instances.
//
// The API key belongs to a sub-user limited to compute, so billing endpoints
// answer 403 and nothing here calls them.
package vultr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const api = "https://api.vultr.com/v2"

// Client talks to the Vultr API.
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

// Region is one Vultr location.
type Region struct {
	ID        string   `json:"id"`
	City      string   `json:"city"`
	Country   string   `json:"country"`
	Continent string   `json:"continent"`
	Options   []string `json:"options"`
}

// Instance is a cloud compute instance.
type Instance struct {
	ID           string   `json:"id"`
	Region       string   `json:"region"`
	Plan         string   `json:"plan"`
	OS           string   `json:"os"`
	OsID         int      `json:"os_id"`
	MainIP       string   `json:"main_ip"`
	V6MainIP     string   `json:"v6_main_ip"`
	Status       string   `json:"status"`
	PowerStatus  string   `json:"power_status"`
	ServerStatus string   `json:"server_status"`
	Label        string   `json:"label"`
	Tags         []string `json:"tags"`
	DateCreated  string   `json:"date_created"`
}

// Ready reports whether the instance is up far enough to connect to.
func (i Instance) Ready() bool {
	return i.Status == "active" && i.PowerStatus == "running" &&
		i.MainIP != "" && i.MainIP != "0.0.0.0"
}

// SSHKey is a public key registered with the account.
type SSHKey struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	SSHKey  string `json:"ssh_key"`
	DateAdd string `json:"date_created"`
}

// CreateRequest describes a new instance.
type CreateRequest struct {
	Region     string   `json:"region"`
	Plan       string   `json:"plan"`
	OsID       int      `json:"os_id"`
	Label      string   `json:"label,omitempty"`
	Hostname   string   `json:"hostname,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	SSHKeys    []string `json:"sshkey_id,omitempty"`
	UserData   string   `json:"user_data,omitempty"`
	EnableIPv6 bool     `json:"enable_ipv6"`
	Backups    string   `json:"backups,omitempty"`
}

// Regions lists every location.
func (c *Client) Regions(ctx context.Context) ([]Region, error) {
	var out struct {
		Regions []Region `json:"regions"`
	}
	if err := c.do(ctx, http.MethodGet, "/regions?per_page=500", nil, &out); err != nil {
		return nil, err
	}
	return out.Regions, nil
}

// Instances lists the account's instances.
func (c *Client) Instances(ctx context.Context) ([]Instance, error) {
	var out struct {
		Instances []Instance `json:"instances"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances?per_page=500", nil, &out); err != nil {
		return nil, err
	}
	return out.Instances, nil
}

// InstancesByTag lists only the instances carrying the given tag, so this
// tool never touches anything it did not create.
func (c *Client) InstancesByTag(ctx context.Context, tag string) ([]Instance, error) {
	var out struct {
		Instances []Instance `json:"instances"`
	}
	path := "/instances?per_page=500&tag=" + url.QueryEscape(tag)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Instances, nil
}

// Instance fetches one instance.
func (c *Client) Instance(ctx context.Context, id string) (*Instance, error) {
	var out struct {
		Instance Instance `json:"instance"`
	}
	if err := c.do(ctx, http.MethodGet, "/instances/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out.Instance, nil
}

// CreateInstance provisions a new instance.
func (c *Client) CreateInstance(ctx context.Context, req CreateRequest) (*Instance, error) {
	var out struct {
		Instance Instance `json:"instance"`
	}
	if err := c.do(ctx, http.MethodPost, "/instances", req, &out); err != nil {
		return nil, err
	}
	return &out.Instance, nil
}

// DeleteInstance destroys an instance.
func (c *Client) DeleteInstance(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/instances/"+id, nil, nil)
}

// SSHKeys lists the registered public keys.
func (c *Client) SSHKeys(ctx context.Context) ([]SSHKey, error) {
	var out struct {
		SSHKeys []SSHKey `json:"ssh_keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/ssh-keys?per_page=500", nil, &out); err != nil {
		return nil, err
	}
	return out.SSHKeys, nil
}

// CreateSSHKey registers a public key and returns its id.
func (c *Client) CreateSSHKey(ctx context.Context, name, pub string) (string, error) {
	in := struct {
		Name   string `json:"name"`
		SSHKey string `json:"ssh_key"`
	}{name, pub}
	var out struct {
		SSHKey SSHKey `json:"ssh_key"`
	}
	if err := c.do(ctx, http.MethodPost, "/ssh-keys", in, &out); err != nil {
		return "", err
	}
	return out.SSHKey.ID, nil
}

// WaitReady polls until the instance is running with an address, or the
// context runs out.
func (c *Client) WaitReady(ctx context.Context, id string) (*Instance, error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		inst, err := c.Instance(ctx, id)
		if err == nil && inst.Ready() {
			return inst, nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return nil, fmt.Errorf("waiting for instance %s: %w", id, err)
			}
			return nil, fmt.Errorf("instance %s did not come up in time", id)
		case <-ticker.C:
		}
	}
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
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vultr %s %s: %s: %s", method, path, resp.Status, errorMessage(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("vultr response is not valid JSON: %w", err)
		}
	}
	return nil
}

func errorMessage(b []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
