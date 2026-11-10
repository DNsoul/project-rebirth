package tunneltest

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
)

// Registration is everything needed to register a device through the
// enrollment peer: where the server is, and the identity every device
// registers under.
type Registration struct {
	Endpoint     netip.AddrPort
	ServerPublic awg.Key
	Params       awg.Params

	// PeerPrivate and PeerAddr are the enrollment peer's identity. It is the
	// same for every device, and the server lets it reach the registration
	// service only.
	PeerPrivate awg.Key
	PeerAddr    netip.Addr

	// Server is where registration answers inside the tunnel. The zero value
	// means the standard place.
	Server netip.AddrPort

	// ContentKey seals the messages.
	ContentKey []byte

	// SelfTest is the key the check registers its device with. On a server that
	// asks the operator before letting devices in, it is the server's
	// preapproved key, the one way past the queue. Nil means a fresh key every
	// time, which is all a server that does not ask needs.
	SelfTest *awg.Keypair

	// Probe is an address the enrollment peer must not be able to reach: the
	// check opens a connection to it through the enrollment peer and fails if it
	// is answered. The zero value means a well-known public address.
	Probe netip.AddrPort

	MTU int
}

// defaultProbe is somewhere on the internet. The enrollment peer must get no
// answer from it, however well the address itself is doing.
var defaultProbe = netip.MustParseAddrPort("1.1.1.1:443")

func (r Registration) asTarget() Target {
	return Target{
		Endpoint:      r.Endpoint,
		ServerPublic:  r.ServerPublic,
		Params:        r.Params,
		ClientPrivate: r.PeerPrivate,
		ClientAddr:    r.PeerAddr,
		MTU:           r.MTU,
	}
}

// Device is a device being registered.
type Device struct {
	ID      []byte // proto.DeviceIDSize bytes
	Keypair awg.Keypair
	Name    string
}

// Register brings the enrollment peer up in memory and registers the device
// through it. It returns the address the server gave the device, with prefix.
//
// The client lives entirely in memory, like the one in Run, so this works from
// a tool or from a server without touching the machine's network.
func Register(ctx context.Context, r Registration, d Device) (string, error) {
	tnet, closeClient, err := connect(r.asTarget())
	if err != nil {
		return "", err
	}
	defer closeClient()

	return enroll.Enroll(ctx, tnet.DialContext, enroll.Request{
		DeviceID:   d.ID,
		PublicKey:  d.Keypair.Public[:],
		Name:       d.Name,
		ContentKey: r.ContentKey,
		Server:     r.Server,
	})
}

// SelfTestDeviceID is the identity a server's check registers under; see
// enroll.SelfTestDeviceID.
func SelfTestDeviceID() []byte { return enroll.SelfTestDeviceID() }

// checkEnrollmentIsContained proves that the key every device is given for
// registering cannot be used for anything else.
//
// That key is published to everyone who has the app, so if the server forwarded
// what comes in through it, the server would be a free exit for anybody holding
// the app. The server's firewall is what prevents that, and a firewall that
// failed to load looks exactly like one that works until somebody uses it. So
// the check first makes sure the peer is connected at all (an unanswered probe
// proves nothing from a tunnel that is down), and then tries to reach somewhere
// it must not.
func checkEnrollmentIsContained(ctx context.Context, t Target) Check {
	const name = "enrollment key is not a way out"
	fail := func(format string, args ...any) Check {
		return Check{Name: name, Required: true, Detail: fmt.Sprintf(format, args...)}
	}

	tnet, closeClient, err := connect(t.Registration.asTarget())
	if err != nil {
		return fail("%v", err)
	}
	defer closeClient()

	if err := pingUntil(ctx, tnet, netip.MustParseAddr("10.8.0.1")); err != nil {
		return fail("the enrollment peer could not reach the server itself, so nothing can be concluded: %v", err)
	}

	probe := t.Registration.Probe
	if !probe.IsValid() {
		probe = defaultProbe
	}
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if c, err := tnet.DialContextTCPAddrPort(dialCtx, probe); err == nil {
		_ = c.Close()
		return fail("the enrollment key reached %s through the server: it is forwarding the key's traffic, "+
			"and anyone with the app could use the server as an exit", probe)
	}
	return Check{Name: name, OK: true, Required: true,
		Detail: fmt.Sprintf("the server answers the enrollment peer, and %s is not reachable through it", probe)}
}

// pingUntil asks an address inside the tunnel until it answers or time is up.
func pingUntil(ctx context.Context, n *netstack.Net, to netip.Addr) error {
	for seq := 1; ; seq++ {
		if ok, err := echoOnce(ctx, n, to, seq); err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no answer from %s: %w", to, ctx.Err())
		default:
		}
	}
}

// checkRegistration does what a new device does: registers through the
// enrollment peer, then connects with the key it just registered and uses the
// resolver. It is the one check that proves a new server can take on devices.
func checkRegistration(ctx context.Context, t Target) Check {
	const name = "device registration"
	fail := func(format string, args ...any) Check {
		return Check{Name: name, Required: true, Detail: fmt.Sprintf(format, args...)}
	}

	var kp awg.Keypair
	if t.Registration.SelfTest != nil {
		kp = *t.Registration.SelfTest
	} else {
		fresh, err := awg.GenerateKeypair()
		if err != nil {
			return fail("%v", err)
		}
		kp = fresh
	}
	addr, err := Register(ctx, *t.Registration, Device{ID: SelfTestDeviceID(), Keypair: kp, Name: "selftest"})
	if err != nil {
		return fail("registering: %v", err)
	}
	prefix, err := netip.ParsePrefix(addr)
	if err != nil {
		return fail("the server answered with %q, which is not an address", addr)
	}

	// Now as the device itself, with the key and address it was just given.
	as := t
	as.ClientPrivate, as.ClientAddr = kp.Private, prefix.Addr()
	tnet, closeClient, err := connect(as)
	if err != nil {
		return fail("%v", err)
	}
	defer closeClient()
	if _, _, err := resolveWithRetry(ctx, tnet, t.Resolver, "one.one.one.one"); err != nil {
		return fail("registered as %s, but its own key got no answer from the resolver: %v", prefix.Addr(), err)
	}
	return Check{Name: name, OK: true, Required: true,
		Detail: fmt.Sprintf("registered as %s and used the tunnel with its own key", prefix.Addr())}
}
