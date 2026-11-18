package tunneltest

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
)

// TraceName is the site whose trace page says how a request reached it.
const TraceName = "www.cloudflare.com"

// Trace asks Cloudflare's trace page through the tunnel, the way an app on a
// device would: the name through the tunnel's resolver, the page through the
// stand-in address it gives. It returns what the page says, among it "ip"
// (where the request left the server from) and "warp" ("on" when through WARP).
func Trace(ctx context.Context, n *netstack.Net, resolver netip.AddrPort) (map[string]string, error) {
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	addrs, _, err := resolveWithRetry(rctx, n, resolver, TraceName)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("look up %s: %w", TraceName, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no address for %s", TraceName)
	}
	standIn := addrs[0]
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return n.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(standIn, 443))
			},
			// What matters is what the page says, not who signed it.
			TLSClientConfig:   &tls.Config{ServerName: TraceName, InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+TraceName+"/cdn-cgi/trace", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the trace page through %s: %w", standIn, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the trace page answered %s", resp.Status)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			out[k] = v
		}
	}
	if out["ip"] == "" {
		return nil, errors.New("the trace page says nothing about where the request came from")
	}
	return out, nil
}
