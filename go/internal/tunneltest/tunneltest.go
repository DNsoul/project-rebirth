// Package tunneltest connects to a server the way a client would and checks
// that the whole path works.
//
// It is how a server proves that its replacement is ready before telling
// anyone about it. The check goes through the real tunnel with the real
// parameters: the handshake, the resolver, the translation of stand-in
// addresses, forwarding and NAT. A replacement that merely reports itself
// healthy has shown nothing a client would care about.
//
// The client runs entirely in memory. amneziawg-go ships a userspace network
// stack, so no tunnel device is created and no privileges are needed, and the
// machine's own routing is never touched.
package tunneltest

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"

	"awgrotator/internal/awg"
)

// Target describes the server under test and the identity to test it with.
type Target struct {
	// Endpoint is the server's public address and port.
	Endpoint netip.AddrPort

	ServerPublic awg.Key
	Params       awg.Params

	// ClientPrivate is the key of a peer the server accepts for testing, and
	// ClientAddr the address it uses inside the tunnel.
	ClientPrivate awg.Key
	ClientAddr    netip.Addr

	// Resolver is the split resolver inside the tunnel, and StandIn the range
	// its stand-in addresses come from.
	Resolver netip.AddrPort
	StandIn  netip.Prefix

	MTU int

	// Keepalive is the client's persistent keepalive in seconds. Zero means the
	// short interval the checks use so that a lost first packet is retried
	// quickly; a long-lived client wants the 25 seconds real clients use.
	Keepalive int

	// Registration, when set, adds a check that a new device can register and
	// use the tunnel with the key it registered.
	Registration *Registration
}

// Options tune the check. The zero value is usable.
type Options struct {
	// ForeignName must resolve to a stand-in. The default is a name on a
	// stable anycast address that serves TLS.
	ForeignName string

	// LocalName should resolve to a real address. It is reported, not
	// required: which view of a site a resolver sees may legitimately vary.
	LocalName string

	// Timeout bounds the whole run. The default is 60 seconds, which covers a
	// handshake on a freshly started server.
	Timeout time.Duration
}

// Check is the outcome of one step.
type Check struct {
	Name     string
	OK       bool
	Required bool
	Detail   string
}

// Report is everything the run found.
type Report struct {
	Checks  []Check
	Elapsed time.Duration
}

// Passed reports whether every required check passed.
func (r Report) Passed() bool {
	required := 0
	for _, c := range r.Checks {
		if !c.Required {
			continue
		}
		required++
		if !c.OK {
			return false
		}
	}
	return required > 0
}

// String renders the report for a log.
func (r Report) String() string {
	var b strings.Builder
	for _, c := range r.Checks {
		mark := "ok  "
		switch {
		case !c.OK && c.Required:
			mark = "FAIL"
		case !c.OK:
			mark = "note"
		}
		fmt.Fprintf(&b, "  [%s] %s", mark, c.Name)
		if c.Detail != "" {
			fmt.Fprintf(&b, ": %s", c.Detail)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "  (%s)", r.Elapsed.Round(10*time.Millisecond))
	return b.String()
}

func (o Options) withDefaults() Options {
	if o.ForeignName == "" {
		o.ForeignName = "one.one.one.one"
	}
	if o.LocalName == "" {
		o.LocalName = "yandex.ru"
	}
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	return o
}

// Run brings a client up against the target and checks the path.
//
// An error means the check could not be carried out at all. A server that
// fails the check is not an error: it is a Report that does not pass.
func Run(ctx context.Context, t Target, opts Options) (Report, error) {
	opts = opts.withDefaults()
	started := time.Now()

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	tnet, closeClient, err := connect(t)
	if err != nil {
		return Report{}, err
	}
	defer closeClient()

	var rep Report
	add := func(c Check) { rep.Checks = append(rep.Checks, c) }
	finish := func() (Report, error) {
		rep.Elapsed = time.Since(started)
		return rep, nil
	}

	// 1. The first query is also what triggers the handshake, so it is
	// retried until it is answered or the time is up.
	addrs, rcode, err := resolveWithRetry(ctx, tnet, t.Resolver, opts.ForeignName)
	if err != nil {
		add(Check{Name: "handshake and resolver", Required: true,
			Detail: fmt.Sprintf("no answer from %s through the tunnel: %v", t.Resolver, err)})
		return finish()
	}
	add(Check{Name: "handshake and resolver", OK: true, Required: true,
		Detail: fmt.Sprintf("answered in %s", time.Since(started).Round(10*time.Millisecond))})

	// 2. A foreign name has to come back as a stand-in.
	if rcode != dnsmessage.RCodeSuccess || len(addrs) == 0 {
		add(Check{Name: "foreign name is tunnelled", Required: true,
			Detail: fmt.Sprintf("%s: rcode %v, %d addresses", opts.ForeignName, rcode, len(addrs))})
		return finish()
	}
	standIn := addrs[0]
	for _, a := range addrs {
		if !t.StandIn.Contains(a) {
			add(Check{Name: "foreign name is tunnelled", Required: true,
				Detail: fmt.Sprintf("%s resolved to %s, which is not a stand-in", opts.ForeignName, a)})
			return finish()
		}
	}
	add(Check{Name: "foreign name is tunnelled", OK: true, Required: true,
		Detail: fmt.Sprintf("%s -> %s", opts.ForeignName, standIn)})

	// 3. And the stand-in has to lead to the real site: this is what proves
	// the translation, forwarding and NAT.
	if detail, err := fetchThrough(ctx, tnet, standIn, opts.ForeignName); err != nil {
		add(Check{Name: "site reachable through the stand-in", Required: true, Detail: err.Error()})
	} else {
		add(Check{Name: "site reachable through the stand-in", OK: true, Required: true, Detail: detail})
	}

	// 4. A local name should not be tunnelled. Informational only.
	if local, _, err := queryA(ctx, tnet, t.Resolver, opts.LocalName); err != nil {
		add(Check{Name: "local name goes direct", Detail: err.Error()})
	} else {
		direct := len(local) > 0
		for _, a := range local {
			if t.StandIn.Contains(a) {
				direct = false
			}
		}
		add(Check{Name: "local name goes direct", OK: direct, Detail: fmt.Sprintf("%s -> %v", opts.LocalName, local)})
	}

	// 5. A new device must be able to register and then use its own key.
	if t.Registration != nil {
		add(checkRegistration(ctx, t))
		add(checkEnrollmentIsContained(ctx, t))
	}
	return finish()
}

// resolveWithRetry asks until it gets an answer. The first packet or two after
// a handshake are lost when S4 is set, which a measurement against real
// servers showed costs about a second, so the first attempt failing is normal
// and the retry is kept short.
func resolveWithRetry(ctx context.Context, n *netstack.Net, server netip.AddrPort, name string) ([]netip.Addr, dnsmessage.RCode, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, time.Second)
		addrs, rcode, err := queryA(attemptCtx, n, server, name)
		cancel()
		if err == nil {
			return addrs, rcode, nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return nil, 0, fmt.Errorf("gave up after %d attempts: %w", attempt+1, lastErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// queryA sends one A query over the tunnel and returns the addresses.
func queryA(ctx context.Context, n *netstack.Net, server netip.AddrPort, name string) ([]netip.Addr, dnsmessage.RCode, error) {
	qname, err := dnsmessage.NewName(strings.TrimSuffix(name, ".") + ".")
	if err != nil {
		return nil, 0, err
	}
	var idBytes [2]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, 0, err
	}
	id := binary.BigEndian.Uint16(idBytes[:])

	query, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: qname, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}).Pack()
	if err != nil {
		return nil, 0, err
	}

	c, err := n.DialUDPAddrPort(netip.AddrPort{}, server)
	if err != nil {
		return nil, 0, err
	}
	defer c.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	if _, err := c.Write(query); err != nil {
		return nil, 0, err
	}
	buf := make([]byte, 4096)
	size, err := c.Read(buf)
	if err != nil {
		return nil, 0, err
	}

	var msg dnsmessage.Message
	if err := msg.Unpack(buf[:size]); err != nil {
		return nil, 0, fmt.Errorf("unreadable reply: %w", err)
	}
	if msg.ID != id || !msg.Response {
		return nil, 0, errors.New("the reply does not match the query")
	}
	var out []netip.Addr
	for _, r := range msg.Answers {
		if a, ok := r.Body.(*dnsmessage.AResource); ok {
			out = append(out, netip.AddrFrom4(a.A))
		}
	}
	return out, msg.RCode, nil
}

// fetchThrough opens TLS to the stand-in with the real name as the server name
// and reads the start of an HTTP reply. The certificate is not verified: the
// point is that bytes make the round trip to a live web server, not who owns
// it.
func fetchThrough(ctx context.Context, n *netstack.Net, standIn netip.Addr, name string) (string, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	raw, err := n.DialContextTCPAddrPort(dialCtx, netip.AddrPortFrom(standIn, 443))
	if err != nil {
		return "", fmt.Errorf("connect to %s:443: %w", standIn, err)
	}
	defer raw.Close()
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}

	tlsConn := tls.Client(raw, &tls.Config{ServerName: name, InsecureSkipVerify: true})
	if err := tlsConn.HandshakeContext(dialCtx); err != nil {
		return "", fmt.Errorf("TLS to %s through %s: %w", name, standIn, err)
	}

	request := "HEAD / HTTP/1.1\r\nHost: " + name + "\r\nConnection: close\r\n\r\n"
	if _, err := tlsConn.Write([]byte(request)); err != nil {
		return "", fmt.Errorf("send the request: %w", err)
	}
	buf := make([]byte, 64)
	size, err := tlsConn.Read(buf)
	if err != nil && size == 0 {
		return "", fmt.Errorf("read the reply: %w", err)
	}
	status, _, _ := strings.Cut(string(buf[:size]), "\r\n")
	if !strings.HasPrefix(status, "HTTP/") {
		return "", fmt.Errorf("not an HTTP reply: %q", status)
	}
	return fmt.Sprintf("%s via %s", status, standIn), nil
}

// Connect brings an in-memory client up and returns its network and a function
// that tears it down. It is for callers that keep a tunnel open for hours, such
// as the soak test, rather than checking and leaving.
func Connect(t Target) (*netstack.Net, func(), error) { return connect(t) }

// connect brings the in-memory client up and returns its network and a
// function that tears it down.
func connect(t Target) (*netstack.Net, func(), error) {
	keepalive := t.Keepalive
	if keepalive == 0 {
		keepalive = 2
	}
	mtu := t.MTU
	if mtu == 0 {
		mtu = awg.MTU
	}
	var dns []netip.Addr
	if t.Resolver.IsValid() {
		dns = append(dns, t.Resolver.Addr())
	}
	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{t.ClientAddr}, dns, mtu)
	if err != nil {
		return nil, nil, fmt.Errorf("create the in-memory network: %w", err)
	}
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))

	uapi := awg.ServerConfig{
		PrivateKey: t.ClientPrivate,
		Params:     t.Params,
		Peers: []awg.Peer{{
			PublicKey:  t.ServerPublic,
			AllowedIPs: []string{"0.0.0.0/0"},
			Endpoint:   t.Endpoint.String(),
			// Frequent keepalives by default so a lost first packet during
			// the handshake is retried quickly.
			Keepalive: keepalive,
		}},
	}.UAPI()
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("configure the in-memory client: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, nil, fmt.Errorf("bring the in-memory client up: %w", err)
	}
	return tnet, dev.Close, nil
}

// HandshakeResult says how a fresh client got on.
type HandshakeResult struct {
	// Elapsed is the time from bringing the client up to the first reply.
	Elapsed time.Duration

	// Requests is how many echo requests were sent before a reply came back.
	// More than one means the first packets were lost.
	Requests int
}

// Handshake brings a fresh client up and measures how long it takes to get a
// first reply from an address inside the tunnel, typically the server's own.
//
// This is a measurement, not a pass/fail check, and it exists because a
// handshake that always needs a retry is invisible to a test that waits long
// enough: everything passes, and every real connection is slow to start.
func Handshake(ctx context.Context, t Target, probe netip.Addr, timeout time.Duration) (HandshakeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	tnet, closeClient, err := connect(t)
	if err != nil {
		return HandshakeResult{}, err
	}
	defer closeClient()

	requests := 0
	for seq := 1; ; seq++ {
		requests++
		ok, err := echoOnce(ctx, tnet, probe, seq)
		if err == nil && ok {
			return HandshakeResult{Elapsed: time.Since(started), Requests: requests}, nil
		}
		select {
		case <-ctx.Done():
			return HandshakeResult{Requests: requests}, fmt.Errorf("no reply from %s after %d requests", probe, requests)
		default:
		}
	}
}

// echoOnce sends one ICMP echo request and waits up to a second for the reply.
func echoOnce(ctx context.Context, n *netstack.Net, to netip.Addr, seq int) (bool, error) {
	pc, err := n.DialPingAddr(netip.Addr{}, to)
	if err != nil {
		return false, err
	}
	defer pc.Close()

	payload := []byte("awg-handshake")
	request, err := (&icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: 0x4157, Seq: seq, Data: payload},
	}).Marshal(nil)
	if err != nil {
		return false, err
	}

	deadline := time.Now().Add(time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = pc.SetDeadline(deadline)

	if _, err := pc.Write(request); err != nil {
		return false, err
	}
	buf := make([]byte, 256)
	size, err := pc.Read(buf)
	if err != nil {
		return false, err
	}
	reply, err := icmp.ParseMessage(1, buf[:size])
	if err != nil {
		return false, err
	}
	echo, ok := reply.Body.(*icmp.Echo)
	return ok && echo.Seq == seq, nil
}
