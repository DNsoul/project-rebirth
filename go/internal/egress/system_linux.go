//go:build linux

package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"awgrotator/internal/warp"
)

// The routing this owns. The table holds one route, the default one through
// the tunnel to WARP; the rules send what is to go through it there.
const (
	DefaultInterface = "warp0"
	DefaultTable     = 51820

	// The tunnel's own address goes through it always, so that it can be
	// checked before any client is sent there.
	prioOwn = 10001
	// The clients: their traffic for anything the main table knows more
	// precisely than "default" (the tunnel subnet itself, the server's own
	// networks) stays there; the rest goes to WARP.
	prioClientsMain = 10010
	prioClientsWARP = 10011

	mtu = 1280
)

// LinuxSystem is the real machine.
type LinuxSystem struct {
	Interface string // DefaultInterface when empty
	Table     int    // DefaultTable when zero
	Subnet    string // the clients' addresses, as the tunnel gives them out
	Log       *slog.Logger

	mu   sync.Mutex
	dev  *device.Device
	addr netip.Addr
}

func (s *LinuxSystem) iface() string {
	if s.Interface != "" {
		return s.Interface
	}
	return DefaultInterface
}

func (s *LinuxSystem) table() string {
	if s.Table != 0 {
		return strconv.Itoa(s.Table)
	}
	return strconv.Itoa(DefaultTable)
}

// Up implements System.
func (s *LinuxSystem) Up(ctx context.Context, a *warp.Account) error {
	if err := a.Validate(); err != nil {
		return err
	}
	addr, err := netip.ParseAddr(a.Address)
	if err != nil {
		return err
	}
	// Whatever a previous run left goes first: a restart must not fail on a
	// device or a rule that is already there.
	_ = s.Down(ctx)

	tdev, err := tun.CreateTUN(s.iface(), mtu)
	if err != nil {
		return fmt.Errorf("create %s: %w", s.iface(), err)
	}
	// Plain WireGuard: no AmneziaWG settings are given, so the library speaks
	// the protocol WARP's servers expect.
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "warp: "))
	uapi := fmt.Sprintf("private_key=%s\nreplace_peers=true\npublic_key=%s\nendpoint=%s\n"+
		"persistent_keepalive_interval=25\nreplace_allowed_ips=true\nallowed_ip=0.0.0.0/0\n",
		a.PrivateKey.Hex(), a.PeerKey.Hex(), a.Endpoint)
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return fmt.Errorf("configure %s: %w", s.iface(), err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return fmt.Errorf("bring %s up: %w", s.iface(), err)
	}
	s.mu.Lock()
	s.dev, s.addr = dev, addr
	s.mu.Unlock()

	steps := [][]string{
		{"ip", "address", "replace", addr.String() + "/32", "dev", s.iface()},
		{"ip", "link", "set", "dev", s.iface(), "mtu", strconv.Itoa(mtu), "up"},
		// Answers to the clients come back in on this interface from addresses
		// the main table sends elsewhere; a strict reverse-path check would
		// drop them.
		{"sysctl", "-q", "-w", "net.ipv4.conf." + s.iface() + ".rp_filter=2"},
		{"ip", "route", "replace", "default", "dev", s.iface(), "table", s.table()},
		{"ip", "rule", "add", "from", addr.String() + "/32", "lookup", s.table(), "priority", strconv.Itoa(prioOwn)},
	}
	for _, args := range steps {
		if err := run(ctx, args...); err != nil {
			_ = s.Down(ctx)
			return err
		}
	}
	return nil
}

// Down implements System.
func (s *LinuxSystem) Down(ctx context.Context) error {
	err := s.Route(ctx, false)
	delRules(ctx, prioOwn)
	_ = run(ctx, "ip", "route", "flush", "table", s.table())
	s.mu.Lock()
	dev := s.dev
	s.dev, s.addr = nil, netip.Addr{}
	s.mu.Unlock()
	if dev != nil {
		dev.Close() // removes the interface
	}
	return err
}

// Route implements System.
func (s *LinuxSystem) Route(ctx context.Context, on bool) error {
	delRules(ctx, prioClientsMain, prioClientsWARP)
	if !on {
		return nil
	}
	if s.Subnet == "" {
		return errors.New("the clients' subnet is not known")
	}
	if err := run(ctx, "ip", "rule", "add", "from", s.Subnet, "lookup", "main",
		"suppress_prefixlength", "0", "priority", strconv.Itoa(prioClientsMain)); err != nil {
		return err
	}
	if err := run(ctx, "ip", "rule", "add", "from", s.Subnet, "lookup", s.table(),
		"priority", strconv.Itoa(prioClientsWARP)); err != nil {
		delRules(ctx, prioClientsMain, prioClientsWARP)
		return err
	}
	return nil
}

// Probe implements System: Cloudflare's trace page, asked from the tunnel's own
// address so that the request goes through WARP whatever the clients do.
func (s *LinuxSystem) Probe(ctx context.Context) error {
	s.mu.Lock()
	addr := s.addr
	s.mu.Unlock()
	if !addr.IsValid() {
		return errors.New("the tunnel to WARP is not up")
	}
	dialer := &net.Dialer{Timeout: 8 * time.Second, LocalAddr: &net.TCPAddr{IP: addr.AsSlice()}}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:       dialer.DialContext,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, warp.TraceURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the check page answered %s", resp.Status)
	}
	if !warp.ThroughWARP(body) {
		return errors.New("the check went out, but not through WARP")
	}
	return nil
}

// Handshake implements System.
func (s *LinuxSystem) Handshake() (time.Time, bool) {
	s.mu.Lock()
	dev := s.dev
	s.mu.Unlock()
	if dev == nil {
		return time.Time{}, false
	}
	out, err := dev.IpcGet()
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "last_handshake_time_sec="); ok {
			if sec, err := strconv.ParseInt(v, 10, 64); err == nil && sec > 0 {
				return time.Unix(sec, 0), true
			}
		}
	}
	return time.Time{}, false
}

// delRules removes every rule with these priorities; "ip rule del" takes one at
// a time and fails once there are none left.
func delRules(ctx context.Context, prios ...int) {
	for _, p := range prios {
		for i := 0; i < 8; i++ {
			if run(ctx, "ip", "rule", "del", "priority", strconv.Itoa(p)) != nil {
				break
			}
		}
	}
}

func run(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
