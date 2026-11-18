// Package warp gets a server its own account with Cloudflare WARP, which the
// node can send its clients' traffic out through.
//
// Why: the server's own address is the one the apps connect to. When the same
// address is also the one the clients' traffic leaves from, anything a client
// app reaches through the tunnel learns where the tunnel goes in; Russian apps
// that look for VPNs report it, and the address is blocked (ntc.party, topic
// 23690). Sent out through WARP, the traffic leaves from Cloudflare's addresses,
// shared by millions, and blocking those for Russian users does not touch the
// way in.
//
// The account is free, needs no e-mail and is made by the same call the WARP
// app makes (the one wgcf uses). Each server makes its own, so nothing ties the
// servers of one line together at Cloudflare.
package warp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"awgrotator/internal/awg"
)

// DefaultAPI is the registration service the WARP app talks to.
const DefaultAPI = "https://api.cloudflareclient.com/v0a2158"

// DefaultAccountPath is where the node keeps its account.
const DefaultAccountPath = "/var/lib/awgnode/warp.json"

// DefaultPort is the port WARP's servers take WireGuard on when the answer does
// not say.
const DefaultPort = 2408

// Account is what the node keeps of its registration: enough to bring the
// tunnel to Cloudflare up again after a restart without registering again.
type Account struct {
	ID         string    `json:"id"`
	Token      string    `json:"token"`
	PrivateKey awg.Key   `json:"private_key"`
	PeerKey    awg.Key   `json:"peer_public_key"`
	Endpoint   string    `json:"endpoint"`       // ip:port
	Host       string    `json:"host,omitempty"` // the name behind it, for the record
	Address    string    `json:"address"`        // this side's address inside WARP
	CreatedAt  time.Time `json:"created_at"`
}

// Validate checks that an account can bring a tunnel up.
func (a *Account) Validate() error {
	switch {
	case a.PrivateKey.IsZero():
		return errors.New("the account has no private key")
	case a.PeerKey.IsZero():
		return errors.New("the account has no key for Cloudflare's side")
	}
	if _, err := netip.ParseAddrPort(a.Endpoint); err != nil {
		return fmt.Errorf("the account's endpoint %q: %w", a.Endpoint, err)
	}
	if addr, err := netip.ParseAddr(a.Address); err != nil || !addr.Is4() {
		return fmt.Errorf("the account's address %q is not an IPv4 address", a.Address)
	}
	return nil
}

// Load reads an account saved by Save. A missing file is (nil, nil).
func Load(path string) (*Account, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &a, nil
}

// Save writes the account readable by root only.
func Save(path string, a *Account) error {
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Client registers accounts.
type Client struct {
	API  string // DefaultAPI when empty
	HTTP *http.Client
	Now  func() time.Time
}

func (c *Client) api() string {
	if c.API != "" {
		return strings.TrimRight(c.API, "/")
	}
	return DefaultAPI
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// regRequest is what the WARP app sends to register a device.
type regRequest struct {
	Key       string `json:"key"`
	InstallID string `json:"install_id"`
	FCMToken  string `json:"fcm_token"`
	TOS       string `json:"tos"`
	Model     string `json:"model"`
	Type      string `json:"type"`
	Locale    string `json:"locale"`
}

// regResponse is the part of the answer that is used.
type regResponse struct {
	ID     string `json:"id"`
	Token  string `json:"token"`
	Config struct {
		Peers []struct {
			PublicKey string `json:"public_key"`
			Endpoint  struct {
				V4    string `json:"v4"`
				Host  string `json:"host"`
				Ports []int  `json:"ports"`
			} `json:"endpoint"`
		} `json:"peers"`
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
			} `json:"addresses"`
		} `json:"interface"`
	} `json:"config"`
}

// Register makes a new account with a fresh key.
func (c *Client) Register(ctx context.Context) (*Account, error) {
	kp, err := awg.GenerateKeypair()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(regRequest{
		Key:    kp.Public.Base64(),
		TOS:    c.now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Model:  "PC",
		Type:   "Linux",
		Locale: "en_US",
	})
	if err != nil {
		return nil, err
	}
	var resp regResponse
	if err := c.call(ctx, http.MethodPost, "/reg", "", body, &resp); err != nil {
		return nil, fmt.Errorf("register with WARP: %w", err)
	}
	a, err := accountFrom(resp, kp.Private)
	if err != nil {
		return nil, err
	}
	a.CreatedAt = c.now().UTC()

	// The WARP app switches the tunnel on for a new device; the account works
	// without it in practice, so a refusal here is not a failure.
	_ = c.call(ctx, http.MethodPatch, "/reg/"+a.ID, a.Token, []byte(`{"warp_enabled":true}`), nil)
	return a, nil
}

func accountFrom(r regResponse, private awg.Key) (*Account, error) {
	if r.ID == "" || len(r.Config.Peers) == 0 {
		return nil, errors.New("WARP answered without a device or a server to talk to")
	}
	p := r.Config.Peers[0]
	peerKey, err := awg.ParseKeyBase64(p.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("WARP's key %q: %w", p.PublicKey, err)
	}
	endpoint, err := endpointOf(p.Endpoint.V4, p.Endpoint.Host, p.Endpoint.Ports)
	if err != nil {
		return nil, err
	}
	a := &Account{
		ID:         r.ID,
		Token:      r.Token,
		PrivateKey: private,
		PeerKey:    peerKey,
		Endpoint:   endpoint,
		Host:       p.Endpoint.Host,
		Address:    strings.TrimSuffix(r.Config.Interface.Addresses.V4, "/32"),
	}
	if err := a.Validate(); err != nil {
		return nil, fmt.Errorf("WARP's answer: %w", err)
	}
	return a, nil
}

// endpointOf picks the address to send to. The answer gives an IPv4 address
// with port 0 and a name with the real port; the address is used, so that the
// tunnel does not depend on a name lookup, with the name's port.
func endpointOf(v4, host string, ports []int) (string, error) {
	port := DefaultPort
	if _, p, err := net.SplitHostPort(host); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			port = n
		}
	} else if len(ports) > 0 && ports[0] > 0 {
		port = ports[0]
	}
	ip := v4
	if h, _, err := net.SplitHostPort(v4); err == nil {
		ip = h
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is4() {
		return "", fmt.Errorf("WARP gave no IPv4 address to send to (%q)", v4)
	}
	return netip.AddrPortFrom(addr, uint16(port)).String(), nil
}

func (c *Client) call(ctx context.Context, method, path, token string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.api()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("User-Agent", "okhttp/3.12.1")
	req.Header.Set("CF-Client-Version", "a-6.30-3596")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, firstLine(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: unexpected answer: %w", method, path, err)
	}
	return nil
}

func firstLine(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// TraceURL is Cloudflare's own page that says how a request reached it. It is
// asked by address, so that no name has to be looked up on the way.
const TraceURL = "https://1.1.1.1/cdn-cgi/trace"

// ThroughWARP reads a trace page and says whether the request came through
// WARP ("warp=on", or "warp=plus" for a paid account).
func ThroughWARP(trace []byte) bool {
	for _, line := range strings.Split(string(trace), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "warp="); ok {
			return v == "on" || v == "plus"
		}
	}
	return false
}
