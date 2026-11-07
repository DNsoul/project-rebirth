package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sample is one line of the probe's own log. Taken together the lines answer
// the question the outside cannot: when the server stopped being reachable,
// was it still alive and still seeing packets?
type Sample struct {
	At string `json:"at"`

	// OutboundOK is whether the server could still open a connection out.
	OutboundOK  bool   `json:"outbound_ok"`
	OutboundMS  int64  `json:"outbound_ms,omitempty"`
	OutboundErr string `json:"outbound_err,omitempty"`

	// SeenAs is the address the outside world reports for us. If the server
	// is null-routed or re-addressed this is where it shows.
	SeenAs  string `json:"seen_as,omitempty"`
	SeenErr string `json:"seen_err,omitempty"`

	// Link counters for the primary interface. If inbound packets stop
	// arriving while the server is healthy, the drop is upstream of us.
	Iface   string `json:"iface,omitempty"`
	RxPkts  uint64 `json:"rx_pkts"`
	TxPkts  uint64 `json:"tx_pkts"`
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`

	Listeners []Stats `json:"listeners"`
}

// CheckOutbound reports whether the server can still reach the internet.
func CheckOutbound(timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", "1.1.1.1:443", timeout)
	if err != nil {
		return 0, err
	}
	conn.Close()
	return time.Since(start), nil
}

// SeenAs asks an outside service what address our traffic appears to come
// from.
func SeenAs(ctx context.Context, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

// LinkCounters reads the packet counters for the first non-loopback
// interface that has carried traffic.
func LinkCounters() (iface string, rxPkts, txPkts, rxBytes, txBytes uint64, err error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return "", 0, 0, 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	var best uint64
	for sc.Scan() {
		line := sc.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "awg") {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 10 {
			continue
		}
		rb, _ := strconv.ParseUint(fields[0], 10, 64)
		rp, _ := strconv.ParseUint(fields[1], 10, 64)
		tb, _ := strconv.ParseUint(fields[8], 10, 64)
		tp, _ := strconv.ParseUint(fields[9], 10, 64)

		// Pick the busiest interface, which is the one facing the internet.
		if rb+tb > best {
			best = rb + tb
			iface, rxPkts, txPkts, rxBytes, txBytes = name, rp, tp, rb, tb
		}
	}
	if iface == "" {
		return "", 0, 0, 0, 0, fmt.Errorf("no candidate interface in /proc/net/dev")
	}
	return iface, rxPkts, txPkts, rxBytes, txBytes, sc.Err()
}

// Collect takes one sample.
func Collect(ctx context.Context, listeners []*Responder) Sample {
	s := Sample{At: time.Now().UTC().Format(time.RFC3339)}

	if d, err := CheckOutbound(5 * time.Second); err == nil {
		s.OutboundOK = true
		s.OutboundMS = d.Milliseconds()
	} else {
		s.OutboundErr = err.Error()
	}

	if addr, err := SeenAs(ctx, 8*time.Second); err == nil {
		s.SeenAs = addr
	} else {
		s.SeenErr = err.Error()
	}

	if iface, rp, tp, rb, tb, err := LinkCounters(); err == nil {
		s.Iface, s.RxPkts, s.TxPkts, s.RxBytes, s.TxBytes = iface, rp, tp, rb, tb
	}

	for _, l := range listeners {
		s.Listeners = append(s.Listeners, l.Stats())
	}
	return s
}

// Append writes one sample as a line of JSON.
func Append(path string, s Sample) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}
