package soak

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/proxy"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"awgrotator/internal/tunneltest"
)

// ErrNoPing is returned by a tunnel that cannot send ICMP.
var ErrNoPing = errors.New("this tunnel cannot send ICMP")

// Tunnel is what the traffic and the probes need of a tunnel, whatever it is
// made of.
type Tunnel interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)

	// Ping sends one ICMP echo through the tunnel.
	Ping(ctx context.Context, to netip.Addr) (time.Duration, error)

	Close()
}

// connector brings a tunnel up.
type connector func(ctx context.Context) (Tunnel, error)

// publicResolver is the name server the tunnels use. It is reached through the
// tunnel, so name lookups travel the way a person's would.
var publicResolver = netip.MustParseAddrPort("1.1.1.1:53")

// awgTunnel is an AmneziaWG client running in this process.
type awgTunnel struct {
	net   *netstack.Net
	close func()
}

func connectAWG(t *AWGTarget) (Tunnel, error) {
	endpoint, err := netip.ParseAddrPort(t.Endpoint)
	if err != nil {
		return nil, err
	}
	addr, err := netip.ParseAddr(t.ClientAddr)
	if err != nil {
		return nil, err
	}
	n, closeFn, err := tunneltest.Connect(tunneltest.Target{
		Endpoint:      endpoint,
		ServerPublic:  t.ServerPublic,
		Params:        t.Params,
		ClientPrivate: t.ClientPrivate,
		ClientAddr:    addr,
		Resolver:      publicResolver,
		// What real clients use, so the keepalive pattern is the usual one.
		Keepalive: 25,
	})
	if err != nil {
		return nil, err
	}
	return &awgTunnel{net: n, close: closeFn}, nil
}

func (t *awgTunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.net.DialContext(ctx, network, addr)
}

func (t *awgTunnel) Close() { t.close() }

func (t *awgTunnel) Ping(ctx context.Context, to netip.Addr) (time.Duration, error) {
	pc, err := t.net.DialPingAddr(netip.Addr{}, to)
	if err != nil {
		return 0, err
	}
	defer pc.Close()

	request, err := (&icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: 0x534f, Seq: 1, Data: []byte("awg-soak")},
	}).Marshal(nil)
	if err != nil {
		return 0, err
	}
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = pc.SetDeadline(deadline)

	started := time.Now()
	if _, err := pc.Write(request); err != nil {
		return 0, err
	}
	buf := make([]byte, 256)
	size, err := pc.Read(buf)
	if err != nil {
		return 0, err
	}
	reply, err := icmp.ParseMessage(1, buf[:size])
	if err != nil {
		return 0, err
	}
	if _, ok := reply.Body.(*icmp.Echo); !ok {
		return 0, fmt.Errorf("not an echo reply: %v", reply.Type)
	}
	return time.Since(started), nil
}

// socksTunnel is a tunnel somewhere else, reached through a SOCKS5 proxy on
// this machine. The tunnel is restarted by a command.
type socksTunnel struct {
	dialer proxy.ContextDialer
}

func newSOCKS(addr string) (Tunnel, error) {
	d, err := proxy.SOCKS5("tcp", addr, nil, &net.Dialer{Timeout: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("the SOCKS5 dialer does not take a context")
	}
	return &socksTunnel{dialer: cd}, nil
}

func (t *socksTunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.dialer.DialContext(ctx, network, addr)
}

func (t *socksTunnel) Ping(context.Context, netip.Addr) (time.Duration, error) {
	return 0, ErrNoPing
}

func (t *socksTunnel) Close() {}

// counted wraps a tunnel so every byte through it is counted.
type counted struct {
	Tunnel
	in, out *atomic.Int64
}

func (c counted) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := c.Tunnel.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, in: c.in, out: c.out}, nil
}

type countingConn struct {
	net.Conn
	in, out *atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.in.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.out.Add(int64(n))
	return n, err
}
