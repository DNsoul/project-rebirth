package awg

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// These tests feed our rendered configuration to the real amneziawg-go
// parser, so the constraints in Validate are checked against the code that
// actually enforces them rather than against our reading of the docs.
//
// No socket is opened: BindUpdate returns early while the device is down, and
// the device is never brought up.

// stubTUN satisfies tun.Device without touching the operating system.
type stubTUN struct {
	events chan tun.Event
	closed chan struct{}
	once   sync.Once
}

func newStubTUN() *stubTUN {
	return &stubTUN{
		events: make(chan tun.Event),
		closed: make(chan struct{}),
	}
}

func (t *stubTUN) File() *os.File { return nil }

func (t *stubTUN) Read([][]byte, []int, int) (int, error) {
	<-t.closed
	return 0, os.ErrClosed
}

func (t *stubTUN) Write(bufs [][]byte, _ int) (int, error) { return len(bufs), nil }
func (t *stubTUN) MTU() (int, error)                       { return MTU, nil }
func (t *stubTUN) Name() (string, error)                   { return "awgtest", nil }
func (t *stubTUN) Events() <-chan tun.Event                { return t.events }
func (t *stubTUN) BatchSize() int                          { return 1 }

func (t *stubTUN) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

func newTestDevice(t *testing.T) *device.Device {
	t.Helper()
	dev := device.NewDevice(newStubTUN(), conn.NewDefaultBind(),
		device.NewLogger(device.LogLevelSilent, "test"))
	t.Cleanup(dev.Close)
	return dev
}

func TestAmneziaWGAcceptsOurServerConfig(t *testing.T) {
	cfg, _, client := testServerConfig(t)
	dev := newTestDevice(t)

	if err := dev.IpcSet(cfg.UAPI()); err != nil {
		t.Fatalf("amneziawg-go rejected our configuration: %v\n%s", err, cfg.UAPI())
	}

	// Read the configuration back and check the peer really landed.
	got, err := dev.IpcGet()
	if err != nil {
		t.Fatalf("IpcGet: %v", err)
	}
	if !strings.Contains(got, "public_key="+client.Public.Hex()) {
		t.Errorf("the peer is missing from the device configuration:\n%s", got)
	}
	if !strings.Contains(got, "listen_port=41820") {
		t.Errorf("listen port did not take:\n%s", got)
	}
}

// If this ever stops failing, amneziawg-go relaxed the rule and Validate can
// be relaxed with it.
func TestAmneziaWGRejectsShortJunkUnderHeaderProtection(t *testing.T) {
	cfg, _, _ := testServerConfig(t)
	cfg.Params.S4 = HeaderNonceSize - 1 // deliberately bypasses Validate

	dev := newTestDevice(t)
	err := dev.IpcSet(cfg.UAPI())
	if err == nil {
		t.Fatal("amneziawg-go accepted a junk prefix shorter than the header nonce")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "headerprotection") {
		t.Logf("rejected, but for an unexpected reason: %v", err)
	}
}

func TestAmneziaWGAcceptsManyGeneratedParameterSets(t *testing.T) {
	// The generator draws from ranges, so one pass only proves one draw.
	for i := 0; i < 15; i++ {
		cfg, _, _ := testServerConfig(t)
		dev := device.NewDevice(newStubTUN(), conn.NewDefaultBind(),
			device.NewLogger(device.LogLevelSilent, "test"))
		err := dev.IpcSet(cfg.UAPI())
		dev.Close()
		if err != nil {
			t.Fatalf("draw %d was rejected: %v\n%s", i, err, cfg.UAPI())
		}
	}
}

func TestAmneziaWGAcceptsOurClientParameters(t *testing.T) {
	// The client side gets the same S values and header protection key, so
	// the same set has to be valid when a client applies it.
	cfg, server, client := testServerConfig(t)
	clientSide := ServerConfig{
		PrivateKey: client.Private,
		ListenPort: 0, // a client does not listen on a fixed port
		Params:     cfg.Params,
		Peers: []Peer{{
			PublicKey:  server.Public,
			AllowedIPs: []string{"0.0.0.0/0"},
		}},
	}

	dev := newTestDevice(t)
	if err := dev.IpcSet(clientSide.UAPI()); err != nil {
		t.Fatalf("the client side of the same parameters was rejected: %v", err)
	}
}
