// Package globalping runs reachability measurements from real networks in a
// chosen country.
//
// It is how this project answers "can anyone in Russia actually reach this
// server", which is a different question from "is the server healthy" and
// cannot be answered from the operator's own connection.
//
// The free tier allows 250 measurement creations per hour per source address;
// the number of probes inside one measurement is not charged against that, so
// it is always better to ask for more probes than to repeat a measurement.
package globalping

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const api = "https://api.globalping.io/v1"

// PollInterval is how often a running measurement is checked.
const PollInterval = 1500 * time.Millisecond

// Client talks to the Globalping API. No credentials are needed.
type Client struct {
	base string
	http *http.Client
}

// New returns a client.
func New() *Client {
	return &Client{base: api, http: &http.Client{Timeout: 30 * time.Second}}
}

// Probe identifies where a measurement ran.
type Probe struct {
	City    string `json:"city"`
	Country string `json:"country"`
	Network string `json:"network"`
	ASN     int    `json:"asn"`
}

// String renders a probe the way it appears in a report.
func (p Probe) String() string {
	if p.Network == "" {
		return p.City
	}
	return p.City + " / " + p.Network
}

// Outcome is one probe's verdict.
type Outcome struct {
	Probe  Probe  `json:"probe"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Result summarises a measurement.
type Result struct {
	ID       string    `json:"id"`
	Reached  int       `json:"reached"`
	Total    int       `json:"total"`
	Outcomes []Outcome `json:"outcomes,omitempty"`
}

// Summary renders the result as "7/10".
func (r Result) Summary() string {
	return fmt.Sprintf("%d/%d", r.Reached, r.Total)
}

// Failures lists the probes that could not reach the target.
func (r Result) Failures() []string {
	var out []string
	for _, o := range r.Outcomes {
		if !o.OK {
			out = append(out, o.Probe.String())
		}
	}
	return out
}

// Limits reports how much of the hourly allowance is left.
func (c *Client) Limits(ctx context.Context) (remaining int, resetSeconds int, err error) {
	var body struct {
		RateLimit struct {
			Measurements struct {
				Create struct {
					Limit     int `json:"limit"`
					Remaining int `json:"remaining"`
					Reset     int `json:"reset"`
				} `json:"create"`
			} `json:"measurements"`
		} `json:"rateLimit"`
	}
	if err := c.get(ctx, "/limits", &body); err != nil {
		return 0, 0, err
	}
	create := body.RateLimit.Measurements.Create
	return create.Remaining, create.Reset, nil
}

// Ping measures ICMP reachability. It answers whether packets reach the
// address at all, independently of any protocol running on it.
func (c *Client) Ping(ctx context.Context, target, country string, limit int) (Result, error) {
	return c.run(ctx, request{
		Type:      "ping",
		Target:    target,
		Limit:     limit,
		Locations: []location{{Country: country}},
		Options:   map[string]any{"packets": 3},
	}, func(r rawResult) (bool, string) {
		// Ping reports "finished" even when every packet was lost, so the
		// loss figure is what decides here.
		if !r.finished() || r.Stats == nil {
			return false, firstLine(r.RawOutput)
		}
		if r.Stats.Loss >= 100 {
			return false, "100% потерь"
		}
		return true, fmt.Sprintf("%.0fms", r.Stats.Avg)
	})
}

// DNSOverUDP sends a real UDP query to the given address and port. This is
// the only way the free tier can generate UDP traffic, which makes it the
// instrument for finding out which UDP ports still carry traffic.
func (c *Client) DNSOverUDP(ctx context.Context, resolver string, port int, name, country string, limit int) (Result, error) {
	return c.run(ctx, request{
		Type:      "dns",
		Target:    name,
		Limit:     limit,
		Locations: []location{{Country: country}},
		Options: map[string]any{
			"resolver": resolver,
			"protocol": "UDP",
			"port":     port,
			"query":    map[string]any{"type": "A"},
		},
	}, func(r rawResult) (bool, string) {
		// Any reply proves the UDP round trip, including a refusal. Only a
		// timeout means nothing came back.
		if !r.finished() {
			return false, "нет ответа"
		}
		detail := r.StatusCodeName
		if len(r.Answers) > 0 {
			detail = r.Answers[0].Value
		}
		if detail == "" {
			detail = fmt.Sprintf("%.0fms", r.totalMS())
		}
		return true, detail
	})
}

// HTTPS opens a TLS connection on the given port. The certificate does not
// have to validate; completing the handshake is the signal.
func (c *Client) HTTPS(ctx context.Context, target string, port int, country string, limit int) (Result, error) {
	return c.run(ctx, request{
		Type:      "http",
		Target:    target,
		Limit:     limit,
		Locations: []location{{Country: country}},
		Options: map[string]any{
			"port":     port,
			"protocol": "HTTPS",
			"request":  map[string]any{"path": "/", "method": "GET"},
		},
	}, func(r rawResult) (bool, string) {
		if !r.finished() {
			return false, firstLine(r.RawOutput)
		}
		if r.StatusCode != nil && *r.StatusCode > 0 {
			return true, fmt.Sprintf("HTTP %d", *r.StatusCode)
		}
		return true, fmt.Sprintf("%.0fms", r.totalMS())
	})
}

type location struct {
	Country string `json:"country,omitempty"`
}

type request struct {
	Type      string         `json:"type"`
	Target    string         `json:"target"`
	Limit     int            `json:"limit"`
	Locations []location     `json:"locations"`
	Options   map[string]any `json:"measurementOptions,omitempty"`
}

type rawStats struct {
	Loss float64 `json:"loss"`
	Rcv  int     `json:"rcv"`
	Avg  float64 `json:"avg"`
}

type rawAnswer struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type rawResult struct {
	Status         string      `json:"status"`
	RawOutput      string      `json:"rawOutput"`
	Stats          *rawStats   `json:"stats"`
	StatusCodeName string      `json:"statusCodeName"`
	StatusCode     *int        `json:"statusCode"`
	Answers        []rawAnswer `json:"answers"`

	// Timings is an array of per-packet entries for ping but an object for
	// dns and http, so it is kept raw and read per measurement type.
	Timings json.RawMessage `json:"timings"`
}

// finished reports whether the probe completed its exchange. A DNS reply of
// NXDOMAIN or an HTTP 403 both count: the point is that packets made the
// round trip. A timeout comes back as "failed" instead.
func (r rawResult) finished() bool { return r.Status == "finished" }

// totalMS reads the duration out of the object form of Timings.
func (r rawResult) totalMS() float64 {
	if len(r.Timings) == 0 {
		return 0
	}
	var obj struct {
		Total float64 `json:"total"`
	}
	if err := json.Unmarshal(r.Timings, &obj); err != nil {
		return 0
	}
	return obj.Total
}

type measurement struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Results []struct {
		Probe  Probe     `json:"probe"`
		Result rawResult `json:"result"`
	} `json:"results"`
}

// verdict decides whether one probe reached the target.
type verdict func(rawResult) (bool, string)

func (c *Client) run(ctx context.Context, req request, judge verdict) (Result, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, "/measurements", req, &created); err != nil {
		return Result{}, err
	}

	for {
		var m measurement
		if err := c.get(ctx, "/measurements/"+created.ID, &m); err != nil {
			return Result{}, err
		}
		if m.Status != "in-progress" {
			res := Result{ID: m.ID, Total: len(m.Results)}
			for _, r := range m.Results {
				ok, detail := judge(r.Result)
				if ok {
					res.Reached++
				}
				res.Outcomes = append(res.Outcomes, Outcome{Probe: r.Probe, OK: ok, Detail: detail})
			}
			return res, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(PollInterval):
		}
	}
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, bytes.NewReader(raw), out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("globalping: hourly measurement allowance is used up")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("globalping %s %s: %s: %s", method, path, resp.Status, firstLine(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("globalping response is not valid JSON: %w", err)
		}
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}
