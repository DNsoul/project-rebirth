package soak

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// A probe address that is stable and answers everything: used inside the
// tunnel, where what matters is that packets come back, not whose they are.
const (
	probeHost = "1.1.1.1"
	probeURL  = "https://www.gstatic.com/generate_204"
)

// probeTunnel tries the tunnel from inside three ways and logs each result: an
// ICMP echo, a TCP connection, and a small HTTPS request. Returns whether the
// HTTPS request worked, which is the test of a tunnel that does its job.
func (a *armRunner) probeTunnel(ctx context.Context, tun Tunnel) bool {
	if rtt, err := tun.Ping(ctx, netip.MustParseAddr(probeHost)); !errors.Is(err, ErrNoPing) {
		a.record("probe", "icmp", rtt, err, true)
	}

	{
		started := time.Now()
		c, err := tun.DialContext(ctx, "tcp", net.JoinHostPort(probeHost, "443"))
		if err == nil {
			_ = c.Close()
		}
		a.record("probe", "tcp", time.Since(started), err, true)
	}

	started := time.Now()
	err := a.httpGet(ctx, tun, probeURL, http.StatusNoContent)
	a.record("probe", "http", time.Since(started), err, true)
	return err == nil
}

// httpGet fetches a URL through the tunnel and checks the status.
func (a *armRunner) httpGet(ctx context.Context, tun Tunnel, url string, want int) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	client := &http.Client{
		Transport: &http.Transport{
			DialContext:         tun.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return fmt.Errorf("status %d, want %d", resp.StatusCode, want)
	}
	return nil
}

// directLoop tries the server outside the tunnel, on this machine's own
// connection, for as long as the run lasts. A server whose address has been
// blocked stops answering here before an established tunnel to it breaks, so
// this is the first sign.
func (a *armRunner) directLoop(ctx context.Context) {
	for {
		a.directOnce(ctx)
		if a.sleep(ctx, a.cfg.DirectEvery.D()) != nil {
			return
		}
	}
}

func (a *armRunner) directOnce(ctx context.Context) {
	for _, port := range a.arm.DirectTCP {
		started := time.Now()
		cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
		c, err := (&net.Dialer{}).DialContext(cctx, "tcp", net.JoinHostPort(a.arm.ServerIP, strconv.Itoa(port)))
		cancel()
		if err == nil {
			_ = c.Close()
		}
		a.record("direct", "tcp/"+strconv.Itoa(port), time.Since(started), err, true)
	}
	rtt, err := systemPing(ctx, a.arm.ServerIP)
	a.record("direct", "icmp", rtt, err, true)
}

// systemPing sends one echo with the operating system's own ping, which needs
// no privileges.
func systemPing(ctx context.Context, ip string) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "ping", "-n", "1", "-w", "4000", ip)
	} else {
		cmd = exec.CommandContext(ctx, "ping", "-c", "1", "-W", "4", ip)
	}
	out, err := cmd.CombinedOutput()
	return parsePing(string(out), err)
}

// pingTime finds the number just before "TTL=" whatever the language: on a
// Russian Windows the words are in a legacy code page and cannot be matched by
// name, but the digits and the TTL are plain ASCII.
var pingTime = regexp.MustCompile(`[=<](\d+)[^\d=]*\s+TTL=`)

// parsePing reads the output of ping in English or Russian. A reply is
// recognised by the TTL, which both print and neither prints for a timeout.
func parsePing(out string, runErr error) (time.Duration, error) {
	if !strings.Contains(strings.ToUpper(out), "TTL=") {
		if runErr != nil {
			return 0, fmt.Errorf("no reply")
		}
		return 0, fmt.Errorf("no reply (%s)", firstLine(out))
	}
	if m := pingTime.FindStringSubmatch(out); m != nil {
		ms, _ := strconv.Atoi(m[1])
		return time.Duration(ms) * time.Millisecond, nil
	}
	return 0, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
