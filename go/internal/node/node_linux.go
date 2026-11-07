//go:build linux

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
	"awgrotator/internal/proto"
)

// natTable is the nftables table this node owns. Nothing else is touched, so
// rules put there by anything else survive.
const natTable = "awgnat"

// StatusInterval is how often the status file is refreshed.
const StatusInterval = 10 * time.Second

// Node owns the tunnel device and the gateway configuration.
type Node struct {
	cfg *Config
	log *slog.Logger

	mu        sync.Mutex
	tdev      tun.Device
	dev       *device.Device
	startedAt time.Time
	split     *splitRuntime

	// Registration of devices; all nil unless the configuration asks for it.
	reg        *enroll.Registry
	enrollStop context.CancelFunc
	enrollDone chan struct{}
	lastFlush  time.Time
}

// New returns a node for the given configuration.
func New(cfg *Config, log *slog.Logger) *Node {
	return &Node{cfg: cfg, log: log}
}

// Start brings everything up: the tunnel device, then the machine's gateway
// configuration. It is safe to call on a machine where a previous run left
// things half configured, because every step replaces rather than appends.
func (n *Node) Start(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.dev != nil {
		return fmt.Errorf("already started")
	}

	n.log.Info("creating tunnel device", "interface", n.cfg.Interface, "mtu", awg.MTU)
	tdev, err := tun.CreateTUN(n.cfg.Interface, awg.MTU)
	if err != nil {
		return fmt.Errorf("create %s: %w", n.cfg.Interface, err)
	}

	dev := device.NewDevice(tdev, conn.NewDefaultBind(),
		device.NewLogger(device.LogLevelError, "awg: "))

	// The devices that registered before this start are accepted from the
	// first moment, so a restart does not cut anybody off.
	sc := n.cfg.ServerConfig()
	if n.cfg.Enroll != nil {
		reg, err := n.openRegistry()
		if err != nil {
			dev.Close()
			return fmt.Errorf("open the device list: %w", err)
		}
		n.reg = reg
		for _, d := range reg.Devices() {
			key, err := enroll.ParseKey(d.PublicKey)
			if err != nil {
				continue // the registry was checked on load; this cannot happen
			}
			sc.Peers = append(sc.Peers, awg.Peer{PublicKey: key, AllowedIPs: []string{d.Address}})
		}
		n.log.Info("loaded the device list", "devices", reg.Len())
	}

	if err := dev.IpcSet(sc.UAPI()); err != nil {
		dev.Close()
		return fmt.Errorf("configure device: %w", err)
	}
	// The UDP socket opens here: while the device is down, setting the
	// listen port only records it.
	if err := dev.Up(); err != nil {
		dev.Close()
		return fmt.Errorf("bring device up: %w", err)
	}

	n.tdev, n.dev = tdev, dev
	n.startedAt = time.Now().UTC()
	n.log.Info("tunnel is listening", "port", n.cfg.ListenPort)

	for _, step := range []struct {
		what string
		fn   func(context.Context) error
	}{
		{"interface address", n.configureInterface},
		{"ip forwarding", n.enableForwarding},
		{"firewall", n.openFirewall},
		{"nat", n.configureNAT},
		{"split resolver", n.startSplit},
		{"device registration", n.startEnroll},
	} {
		if err := step.fn(ctx); err != nil {
			n.stopEnroll()
			if n.split != nil {
				n.split.stop()
				n.split = nil
			}
			dev.Close()
			n.dev, n.tdev = nil, nil
			return fmt.Errorf("%s: %w", step.what, err)
		}
		n.log.Info("configured", "step", step.what)
	}
	return nil
}

// Close tears the device down. The nftables table is left in place: systemd
// restarts the node, and a gap in NAT would be worse than a stale rule.
func (n *Node) Close() error {
	// Registration goes first of all: it adds peers to the device, and the
	// device is about to go away. It is stopped before the lock is taken,
	// because a request being answered right now needs the lock to reach the
	// device, and stopping waits for it.
	n.stopEnroll()

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dev == nil {
		return nil
	}
	// The resolver goes next: it writes the mapping table one last time on
	// the way out, and it must not be answering queries through a device that
	// no longer exists.
	if n.split != nil {
		n.split.stop()
		n.split = nil
	}
	n.dev.Close() // closes the tun device too
	n.dev, n.tdev = nil, nil
	return nil
}

// Status reads the live device state.
func (n *Node) Status() (Status, error) {
	n.mu.Lock()
	dev := n.dev
	started := n.startedAt
	n.mu.Unlock()

	if dev == nil {
		return Status{}, fmt.Errorf("not started")
	}
	raw, err := dev.IpcGet()
	if err != nil {
		return Status{}, fmt.Errorf("read device state: %w", err)
	}
	port, peers, err := ParseDeviceStatus(raw)
	if err != nil {
		return Status{}, err
	}
	return Status{
		Interface:  n.cfg.Interface,
		ListenPort: port,
		StartedAt:  started,
		UpdatedAt:  time.Now().UTC(),
		Peers:      peers,
		Split:      n.splitStatus(),
	}, nil
}

// WriteStatus refreshes the status file.
func (n *Node) WriteStatus() error {
	st, err := n.Status()
	if err != nil {
		return err
	}
	n.noteDevices(st)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := n.cfg.Status()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Run starts the node and keeps the status file fresh until the context ends.
func (n *Node) Run(ctx context.Context) error {
	if err := n.Start(ctx); err != nil {
		return err
	}
	defer n.Close()

	ticker := time.NewTicker(StatusInterval)
	defer ticker.Stop()

	if err := n.WriteStatus(); err != nil {
		n.log.Warn("could not write status", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			n.log.Info("shutting down")
			return nil
		case <-ticker.C:
			if err := n.WriteStatus(); err != nil {
				n.log.Warn("could not write status", "error", err)
			}
		}
	}
}

// ---- device registration

// DeviceListFlushInterval is how often last-seen times are written to disk.
// They only decide which stale device gives up its place when the list is full,
// so they do not need to be fresh.
const DeviceListFlushInterval = 5 * time.Minute

// openRegistry reads the device list.
//
// A list that cannot be read must not keep the server from starting: a server
// that does not start serves nobody, while a device that is missing from the
// list simply registers again. The unreadable file is set aside, not deleted.
func (n *Node) openRegistry() (*enroll.Registry, error) {
	ec := n.cfg.Enroll
	pool, err := n.cfg.Pool()
	if err != nil {
		return nil, err
	}
	opts := enroll.Options{MaxDevices: ec.MaxDevices}
	path := ec.Registry()

	reg, err := enroll.LoadRegistry(path, pool, opts)
	if err == nil {
		return reg, nil
	}
	aside := fmt.Sprintf("%s.bad-%d", path, time.Now().Unix())
	if renameErr := os.Rename(path, aside); renameErr != nil {
		return nil, fmt.Errorf("%w (and it could not be set aside: %v)", err, renameErr)
	}
	n.log.Error("the device list could not be read; it was set aside and the server starts with none, "+
		"so devices will register again", "error", err, "kept_as", aside)
	return enroll.NewRegistry(path, pool, opts), nil
}

// startEnroll opens the registration service. It listens on the server's own
// tunnel address, so it is reachable only from inside the tunnel.
func (n *Node) startEnroll(context.Context) error {
	ec := n.cfg.Enroll
	if ec == nil {
		return nil
	}
	key, err := proto.Decode(ec.ContentKey, proto.ContentKeySize)
	if err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp4", ec.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", ec.Listen, err)
	}
	srv := &enroll.Server{
		Registry:   n.reg,
		Peers:      n,
		ContentKey: key,
		From:       netip.MustParseAddr(ec.PeerAddress),
		Log:        n.log.With("component", "enroll"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx, pc); err != nil {
			n.log.Error("device registration stopped", "error", err)
		}
	}()
	n.enrollStop, n.enrollDone = cancel, done
	return nil
}

// stopEnroll ends the service and saves the device list.
func (n *Node) stopEnroll() {
	if n.enrollStop != nil {
		n.enrollStop()
		<-n.enrollDone
		n.enrollStop, n.enrollDone = nil, nil
	}
	if n.reg != nil {
		if err := n.reg.Flush(); err != nil {
			n.log.Warn("could not save the device list", "error", err)
		}
	}
}

// SetPeer implements enroll.PeerSink: it adds a device to the running tunnel
// without touching anyone else's connection.
func (n *Node) SetPeer(pub awg.Key, allowedIP string) error {
	dev, err := n.device()
	if err != nil {
		return err
	}
	return dev.IpcSet(fmt.Sprintf("public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s\n", pub.Hex(), allowedIP))
}

// RemovePeer implements enroll.PeerSink.
func (n *Node) RemovePeer(pub awg.Key) error {
	dev, err := n.device()
	if err != nil {
		return err
	}
	return dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", pub.Hex()))
}

func (n *Node) device() (*device.Device, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.dev == nil {
		return nil, fmt.Errorf("the tunnel is not up")
	}
	return n.dev, nil
}

// noteDevices records when each registered device was last connected.
func (n *Node) noteDevices(st Status) {
	if n.reg == nil {
		return
	}
	for _, p := range st.Peers {
		if p.LastHandshake.IsZero() {
			continue
		}
		if key, err := awg.ParseKeyBase64(p.PublicKey); err == nil {
			n.reg.Touch(enroll.KeyString(key), p.LastHandshake)
		}
	}
	if time.Since(n.lastFlush) >= DeviceListFlushInterval {
		n.lastFlush = time.Now()
		if err := n.reg.Flush(); err != nil {
			n.log.Warn("could not save the device list", "error", err)
		}
	}
}

func (n *Node) configureInterface(ctx context.Context) error {
	// "address replace" rather than "add" so a restart does not fail on an
	// address that is already there.
	if err := n.run(ctx, "ip", "address", "replace", n.cfg.Address, "dev", n.cfg.Interface); err != nil {
		return err
	}
	return n.run(ctx, "ip", "link", "set", "dev", n.cfg.Interface,
		"mtu", strconv.Itoa(awg.MTU), "up")
}

// openFirewall makes sure the tunnel port and the forwarded traffic are not
// dropped by whatever firewall the image happens to ship with.
//
// This step is easy to forget and expensive to debug. The Vultr Debian image
// comes with ufw enabled and a default policy of DROP on both input and
// forward, so a node that configures only NAT ends up listening on a port
// nothing outside can reach: ping and ssh keep working, the server looks
// perfectly healthy, and the client retries the handshake forever. It looks
// exactly like censorship and is not.
func (n *Node) openFirewall(ctx context.Context) error {
	if _, err := exec.LookPath("ufw"); err != nil {
		n.log.Info("ufw is not installed; relying on the default policy")
		return n.warnIfInputDropped(ctx)
	}
	out, err := exec.CommandContext(ctx, "ufw", "status").CombinedOutput()
	if err != nil {
		return fmt.Errorf("ufw status: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "Status: active") {
		n.log.Info("ufw is installed but inactive; relying on the default policy")
		return n.warnIfInputDropped(ctx)
	}

	port := strconv.Itoa(n.cfg.ListenPort)
	steps := [][]string{
		// The tunnel's own port.
		{"allow", port + "/udp"},
		// Traffic arriving through the tunnel and being routed onward. Only
		// the tunnel side is allowed in, so the server does not become an
		// open router for the internet.
		{"route", "allow", "in", "on", n.cfg.Interface},
	}
	if ec := n.cfg.Enroll; ec != nil {
		// Registration is addressed to the server itself, so it is input,
		// and only from the tunnel and only to its own address and port.
		if ap, err := netip.ParseAddrPort(ec.Listen); err == nil {
			steps = append(steps, []string{"allow", "in", "on", n.cfg.Interface,
				"to", ap.Addr().String(), "port", strconv.Itoa(int(ap.Port())), "proto", "udp"})
		}
	}
	if split := n.cfg.Split; split != nil {
		// Queries to the resolver are addressed to the server itself, so
		// they are input rather than forwarded traffic. Allowed from the
		// tunnel only, and only to the resolver's own address and port.
		for _, proto := range []string{"udp", "tcp"} {
			steps = append(steps, []string{"allow", "in", "on", n.cfg.Interface,
				"to", split.DNSAddress, "port", "53", "proto", proto})
		}
	}
	for _, args := range steps {
		if err := n.run(ctx, "ufw", args...); err != nil {
			return err
		}
	}
	if err := n.run(ctx, "ufw", "reload"); err != nil {
		return err
	}
	n.log.Info("opened the firewall", "port", n.cfg.ListenPort, "interface", n.cfg.Interface)
	return nil
}

// warnIfInputDropped looks for a default-drop policy we have not handled, so
// an unreachable port shows up in the log instead of being a mystery.
func (n *Node) warnIfInputDropped(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "iptables", "-S").CombinedOutput()
	if err != nil {
		return nil // nothing to learn from here
	}
	for _, policy := range []string{"-P INPUT DROP", "-P FORWARD DROP"} {
		if strings.Contains(string(out), policy) {
			n.log.Warn("a default drop policy is in place and no firewall tool was recognised; "+
				"the tunnel port may be unreachable", "policy", policy)
		}
	}
	return nil
}

func (n *Node) enableForwarding(ctx context.Context) error {
	const conf = "/etc/sysctl.d/99-awgnode.conf"
	if err := os.WriteFile(conf, []byte("net.ipv4.ip_forward=1\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", conf, err)
	}
	return n.run(ctx, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")
}

// configureNAT installs the masquerade rule for the tunnel subnet.
//
// Declaring the table, deleting it and declaring it again makes the script
// idempotent: the first line creates the table when it is missing so the
// delete always has something to remove.
func (n *Node) configureNAT(ctx context.Context) error {
	script := fmt.Sprintf(`table ip %[1]s {}
delete table ip %[1]s
table ip %[1]s {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr %[2]s oifname != %[3]q masquerade
	}
%[4]s}
`, natTable, n.cfg.Subnet, n.cfg.Interface, n.cfg.Enroll.ForwardGuard())

	return n.runStdin(ctx, script, "nft", "-f", "-")
}

func (n *Node) run(ctx context.Context, name string, args ...string) error {
	return n.runStdin(ctx, "", name, args...)
}

func (n *Node) runStdin(ctx context.Context, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, trimmed)
	}
	if trimmed != "" {
		n.log.Debug("command output", "cmd", name, "output", trimmed)
	}
	return nil
}
