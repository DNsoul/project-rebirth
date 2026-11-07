package tunneltest

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/netstack"

	"awgrotator/internal/awg"
	"awgrotator/internal/proto"
)

// These tests use the same in-process server as the rest of the package: a real
// AmneziaWG device and real clients over loopback UDP, with an in-memory network
// on each side. Nothing needs a TUN device or privileges, and the packets are
// the real thing: the handshake with the enrollment peer, the registration
// datagram inside the tunnel, a peer added to a running device, and a second
// handshake with the key that was just registered.
//
// What cannot be covered here is the server's firewall, which is Linux.

var e2eContentKey = bytes.Repeat([]byte{0x5A}, proto.ContentKeySize)

// deviceSink puts peers into the running device the way the node does. The
// strings are the same as in node.SetPeer and node.RemovePeer.
type deviceSink struct{ dev *device.Device }

func (s deviceSink) SetPeer(pub awg.Key, ip string) error {
	return s.dev.IpcSet(fmt.Sprintf("public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s\n", pub.Hex(), ip))
}

func (s deviceSink) RemovePeer(pub awg.Key) error {
	return s.dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", pub.Hex()))
}

var registrarServer = serverOptions{
	registrar: true,
	answers: map[string]string{
		"one.one.one.one": "198.18.0.5",
		"yandex.ru":       "5.255.255.77",
	},
}

// asDevice is the target as seen by a registered device: its own key and the
// address it was given.
func (r rig) asDevice(key awg.Key, addr netip.Addr) Target {
	t := r.target
	t.ClientPrivate, t.ClientAddr = key, addr
	return t
}

func e2eDevice(t *testing.T, id byte, name string) Device {
	t.Helper()
	kp, err := awg.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	return Device{ID: bytes.Repeat([]byte{id}, proto.DeviceIDSize), Keypair: kp, Name: name}
}

func registerE2E(ctx context.Context, t *testing.T, r rig, d Device) netip.Addr {
	t.Helper()
	got, err := Register(ctx, r.reg, d)
	if err != nil {
		t.Fatalf("registering %s: %v", d.Name, err)
	}
	prefix, err := netip.ParsePrefix(got)
	if err != nil {
		t.Fatalf("registering %s: %q is not an address", d.Name, got)
	}
	return prefix.Addr()
}

// pingServer asks the server's tunnel address until it answers or time is up.
func pingServer(ctx context.Context, n *netstack.Net) error {
	return pingUntil(ctx, n, netip.MustParseAddr("10.8.0.1"))
}

func within(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// The promise of the whole design: every device has its own key and its own
// address, and being online together does not make them take turns.
func TestTwoDevicesRegisterAndStayOnlineTogether(t *testing.T) {
	r := startServer(t, registrarServer)
	ctx, cancel := within(60 * time.Second)
	defer cancel()

	windows, phone := e2eDevice(t, 1, "windows"), e2eDevice(t, 2, "phone")
	wAddr := registerE2E(ctx, t, r, windows)
	pAddr := registerE2E(ctx, t, r, phone)
	if wAddr == pAddr {
		t.Fatalf("two devices were given the same address, %s", wAddr)
	}

	wNet, closeW, err := connect(r.asDevice(windows.Keypair.Private, wAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer closeW()
	pNet, closeP, err := connect(r.asDevice(phone.Keypair.Private, pAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer closeP()

	if err := pingServer(ctx, wNet); err != nil {
		t.Fatalf("windows: %v", err)
	}
	if err := pingServer(ctx, pNet); err != nil {
		t.Fatalf("phone: %v", err)
	}

	// Alternate for a while. With a shared key the server would follow
	// whichever spoke last and the other would go quiet.
	for round := 0; round < 6; round++ {
		for name, n := range map[string]*netstack.Net{"windows": wNet, "phone": pNet} {
			roundCtx, roundCancel := within(5 * time.Second)
			err := pingServer(roundCtx, n)
			roundCancel()
			if err != nil {
				t.Fatalf("round %d, %s: %v", round, name, err)
			}
		}
	}
}

func TestRegisteringAgainReplacesTheKeyAndKeepsTheAddress(t *testing.T) {
	r := startServer(t, registrarServer)
	ctx, cancel := within(60 * time.Second)
	defer cancel()

	before := e2eDevice(t, 7, "windows")
	addr := registerE2E(ctx, t, r, before)

	// The same device after a reinstall: same identifier, new key.
	after := e2eDevice(t, 7, "windows")
	if again := registerE2E(ctx, t, r, after); again != addr {
		t.Errorf("the address moved from %s to %s", addr, again)
	}
	if r.registry.Len() != 1 {
		t.Errorf("%d devices registered after a reinstall, want 1", r.registry.Len())
	}

	// The new key works.
	newNet, closeNew, err := connect(r.asDevice(after.Keypair.Private, addr))
	if err != nil {
		t.Fatal(err)
	}
	defer closeNew()
	if err := pingServer(ctx, newNet); err != nil {
		t.Fatalf("the new key: %v", err)
	}

	// The old one does not any more.
	oldNet, closeOld, err := connect(r.asDevice(before.Keypair.Private, addr))
	if err != nil {
		t.Fatal(err)
	}
	defer closeOld()
	short, shortCancel := within(4 * time.Second)
	defer shortCancel()
	if err := pingServer(short, oldNet); err == nil {
		t.Fatal("the key of the replaced installation still gets through")
	}
}

func TestSomeoneWithoutTheContentKeyCannotRegister(t *testing.T) {
	r := startServer(t, registrarServer)
	ctx, cancel := within(5 * time.Second)
	defer cancel()

	reg := r.reg
	reg.ContentKey = bytes.Repeat([]byte{0x11}, proto.ContentKeySize)
	if _, err := Register(ctx, reg, e2eDevice(t, 3, "stranger")); err == nil {
		t.Fatal("a device that does not hold the content key was registered")
	}
	if r.registry.Len() != 0 {
		t.Error("something was recorded for it")
	}
}

// A device that was never registered cannot use the tunnel, whatever it knows.
func TestAnUnregisteredKeyGetsNowhere(t *testing.T) {
	r := startServer(t, registrarServer)
	stranger := e2eDevice(t, 4, "stranger")
	n, closeN, err := connect(r.asDevice(stranger.Keypair.Private, netip.MustParseAddr("10.8.0.9")))
	if err != nil {
		t.Fatal(err)
	}
	defer closeN()
	ctx, cancel := within(4 * time.Second)
	defer cancel()
	if err := pingServer(ctx, n); err == nil {
		t.Fatal("a key nobody registered got through")
	}
}

// What a successor runs before it is published.
func TestTheSelfCheckPassesAgainstAServerThatRegistersDevices(t *testing.T) {
	r := startServer(t, registrarServer)
	ctx, cancel := within(60 * time.Second)
	defer cancel()

	target := r.target
	reg := r.reg
	target.Registration = &reg

	for run := 1; run <= 2; run++ {
		check := checkRegistration(ctx, target)
		if !check.OK || !check.Required {
			t.Fatalf("run %d: %+v", run, check)
		}
	}
	if r.registry.Len() != 1 {
		t.Errorf("%d devices after two checks, want just the one self-test device", r.registry.Len())
	}
	if got := r.registry.Devices()[0].Name; got != "selftest" {
		t.Errorf("the entry is called %q", got)
	}
}

// The check is part of Run, so it is what the manager actually executes.
func TestRunIncludesTheRegistrationCheckWhenAsked(t *testing.T) {
	r := startServer(t, registrarServer)
	ctx, cancel := within(90 * time.Second)
	defer cancel()

	target := r.target
	reg := r.reg
	target.Registration = &reg

	rep, err := Run(ctx, target, Options{Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	check, ok := find(rep, "device registration")
	if !ok || !check.OK {
		t.Fatalf("no passing registration check in the report:\n%s", rep)
	}
	if !rep.Passed() {
		t.Errorf("the report does not pass:\n%s", rep)
	}

	// Without a registration in the target, the check is not run at all.
	target.Registration = nil
	rep, err = Run(ctx, target, Options{Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := find(rep, "device registration"); ok {
		t.Error("the registration check ran although none was asked for")
	}
}

// A server that cannot take on devices must not pass: this is the check that
// stops it being published.
func TestAServerThatDoesNotRegisterDevicesFailsTheReport(t *testing.T) {
	opts := registrarServer
	opts.registrar = false
	r := startServer(t, opts)
	ctx, cancel := within(30 * time.Second)
	defer cancel()

	target := r.target
	reg := r.reg
	target.Registration = &reg

	// Only registration is under test; the rest of the path works.
	rep, err := Run(ctx, target, Options{Timeout: 12 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	check, ok := find(rep, "device registration")
	if !ok || check.OK {
		t.Fatalf("registration should have failed:\n%s", rep)
	}
	if !check.Required || rep.Passed() {
		t.Errorf("a failed registration does not fail the report, so it would not stop a broken server being published:\n%s", rep)
	}
}

// The key every device registers with is public, so it must not reach any
// further than the server itself. In this rig nothing answers on the public
// internet, so the probe is unanswered, as it is behind the real firewall.
func TestTheEnrollmentKeyIsContained(t *testing.T) {
	r := startServer(t, registrarServer)
	ctx, cancel := within(30 * time.Second)
	defer cancel()

	target := r.target
	reg := r.reg
	target.Registration = &reg

	check := checkEnrollmentIsContained(ctx, target)
	if !check.OK || !check.Required {
		t.Fatalf("%+v", check)
	}
}

// And the check must be able to tell when it is not: here the probe address is
// one the server answers on, standing in for a firewall that failed to load.
func TestALeakingEnrollmentKeyFailsTheReport(t *testing.T) {
	r := startServer(t, registrarServer)
	ctx, cancel := within(60 * time.Second)
	defer cancel()

	target := r.target
	reg := r.reg
	reg.Probe = netip.AddrPortFrom(standInAddr, 443) // the rig's "site" answers here
	target.Registration = &reg

	check := checkEnrollmentIsContained(ctx, target)
	if check.OK {
		t.Fatalf("a key that reaches beyond the server passed: %+v", check)
	}
	if !check.Required {
		t.Error("a leak is not marked as required, so it would not stop the server being published")
	}

	rep, err := Run(ctx, target, Options{Timeout: 50 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed() {
		t.Errorf("the report passes although the enrollment key reaches beyond the server:\n%s", rep)
	}
}

// If the enrollment peer cannot even reach the server, an unanswered probe
// means nothing, and the check must say so instead of passing.
func TestTheContainmentCheckDoesNotPassFromADeadTunnel(t *testing.T) {
	opts := registrarServer
	r := startServer(t, opts)
	ctx, cancel := within(6 * time.Second)
	defer cancel()

	reg := r.reg
	reg.PeerPrivate = e2eDevice(t, 9, "wrong").Keypair.Private // not the enrollment key
	target := r.target
	target.Registration = &reg

	if check := checkEnrollmentIsContained(ctx, target); check.OK {
		t.Fatalf("passed although the tunnel never came up: %+v", check)
	}
}
