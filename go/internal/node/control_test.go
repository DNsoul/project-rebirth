package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
	"awgrotator/internal/proto"
)

type sinkTunnel struct {
	mu    sync.Mutex
	peers map[awg.Key]string
}

func (s *sinkTunnel) SetPeer(k awg.Key, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peers == nil {
		s.peers = map[awg.Key]string{}
	}
	s.peers[k] = ip
	return nil
}

func (s *sinkTunnel) RemovePeer(k awg.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.peers, k)
	return nil
}

var ctlContent = bytes.Repeat([]byte{3}, proto.ContentKeySize)

func fill(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

// controlRig is a registration server that asks the operator, with a
// controller over it and a device that has asked to come in.
func controlRig(t *testing.T) (*Controller, *enroll.Server, *time.Time) {
	t.Helper()
	pool, err := enroll.NewPool("10.8.0.0/24", 2, 249)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	srv := &enroll.Server{
		Registry:   enroll.NewRegistry(filepath.Join(dir, "r.json"), pool, enroll.Options{}),
		Peers:      &sinkTunnel{},
		ContentKey: ctlContent,
		From:       netip.MustParseAddr("10.8.0.251"),
		Access:     enroll.NewAccess(filepath.Join(dir, "a.json"), enroll.AccessOptions{}),
		Approval:   true,
		Now:        func() time.Time { return now },
	}
	req := &proto.EnrollRequest{
		V: proto.FormatVersion, Kind: proto.KindEnroll,
		DeviceID: proto.Encode(fill(1, proto.DeviceIDSize)), PublicKey: proto.Encode(fill(0x11, proto.TunnelKeySize)),
		Nonce: proto.Encode(fill(0xAA, proto.EnrollNonceSize)), IssuedAt: now.Unix(), Name: "Xiaomi Redmi Note 12",
	}
	blob, err := req.Seal(ctlContent)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := proto.OpenEnrollResponse(ctlContent, string(srv.Handle(netip.MustParseAddr("10.8.0.251"), []byte(blob))), req.Nonce)
	if err != nil || reply.Error != proto.EnrollPending {
		t.Fatalf("the device should be waiting: %+v %v", reply, err)
	}
	ctl := &Controller{Srv: srv, Now: func() time.Time { return now }}
	return ctl, srv, &now
}

func TestTheOverviewShowsTheWaitingDeviceWithItsCode(t *testing.T) {
	ctl, _, _ := controlRig(t)
	resp := ctl.Handle(ControlRequest{Op: OpOverview})
	if !resp.OK || resp.Overview == nil {
		t.Fatalf("resp = %+v", resp)
	}
	o := resp.Overview
	if !o.Approval || len(o.Pending) != 1 || len(o.Devices) != 0 {
		t.Fatalf("overview = %+v", o)
	}
	if want := proto.ApprovalCode(fill(0x11, proto.TunnelKeySize)); o.Pending[0].Code != want {
		t.Errorf("code %q, want %q", o.Pending[0].Code, want)
	}
	if o.Pending[0].Name != "Xiaomi Redmi Note 12" {
		t.Errorf("name %q", o.Pending[0].Name)
	}
}

func TestApprovedDevicesCarryWhatTheTunnelKnows(t *testing.T) {
	ctl, _, now := controlRig(t)
	id := proto.Encode(fill(1, proto.DeviceIDSize))
	if resp := ctl.Handle(ControlRequest{Op: OpApprove, ID: id}); !resp.OK || resp.Name != "Xiaomi Redmi Note 12" {
		t.Fatalf("approve: %+v", resp)
	}
	shook := now.Add(-30 * time.Second)
	ctl.Peers = func() ([]PeerStatus, error) {
		return []PeerStatus{{
			PublicKey:     base64.StdEncoding.EncodeToString(fill(0x11, proto.TunnelKeySize)),
			LastHandshake: shook, RxBytes: 100, TxBytes: 200,
		}}, nil
	}
	o := ctl.Handle(ControlRequest{Op: OpOverview}).Overview
	if len(o.Devices) != 1 {
		t.Fatalf("devices = %+v", o.Devices)
	}
	d := o.Devices[0]
	if !d.Online || d.LastHandshake != shook.Unix() || d.RxBytes != 100 || d.TxBytes != 200 {
		t.Errorf("device = %+v", d)
	}
}

func TestADecisionOnADeviceThatIsNotThereSaysItIsMissing(t *testing.T) {
	ctl, _, _ := controlRig(t)
	id := proto.Encode(fill(1, proto.DeviceIDSize))
	if resp := ctl.Handle(ControlRequest{Op: OpDeny, ID: id}); !resp.OK {
		t.Fatalf("deny: %+v", resp)
	}
	// The same button pressed twice, or in two chats.
	resp := ctl.Handle(ControlRequest{Op: OpDeny, ID: id})
	if resp.OK || !resp.Missing {
		t.Fatalf("second deny: %+v", resp)
	}
	if resp := ctl.Handle(ControlRequest{Op: "explode"}); resp.OK || resp.Error == "" {
		t.Fatalf("unknown op: %+v", resp)
	}
}

func TestTheOperatorIsToldOnlyOnceThroughTheSocket(t *testing.T) {
	ctl, _, _ := controlRig(t)
	id := proto.Encode(fill(1, proto.DeviceIDSize))
	if resp := ctl.Handle(ControlRequest{Op: OpNotified, IDs: []string{id}}); !resp.OK {
		t.Fatalf("notified: %+v", resp)
	}
	if !ctl.Handle(ControlRequest{Op: OpOverview}).Overview.Pending[0].Notified {
		t.Fatal("the request was not marked")
	}
}

func TestTheSocketCarriesRequestsBothWays(t *testing.T) {
	ctl, _, _ := controlRig(t)
	dir, err := os.MkdirTemp("", "ctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets are not available here: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ctl.ServeControl(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer callCancel()
	resp, err := Control(callCtx, path, ControlRequest{Op: OpOverview})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Overview == nil || len(resp.Overview.Pending) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	id := proto.Encode(fill(1, proto.DeviceIDSize))
	resp, err = Control(callCtx, path, ControlRequest{Op: OpApprove, ID: id})
	if err != nil || !resp.OK {
		t.Fatalf("approve over the socket: %+v %v", resp, err)
	}
}

func TestControlSaysWhenTheNodeIsNotThere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Control(ctx, filepath.Join(t.TempDir(), "none.sock"), ControlRequest{Op: OpOverview}); err == nil {
		t.Fatal("no error for a socket that does not exist")
	}
}
