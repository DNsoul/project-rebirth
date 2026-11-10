package enroll

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"awgrotator/internal/proto"
)

// approvalRig is a server that asks the operator before letting devices in.
func approvalRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, Options{})
	r.srv.Access = NewAccess(filepath.Join(t.TempDir(), "access.json"), AccessOptions{})
	r.srv.Approval = true
	return r
}

func idString(b byte) string  { return b64(b, proto.DeviceIDSize) }
func keyString(b byte) string { return b64(b, proto.TunnelKeySize) }

func wantReason(t *testing.T, reply *proto.EnrollResponse, reason string) {
	t.Helper()
	if reply == nil {
		t.Fatalf("no reply, want %q", reason)
	}
	if reply.Error != reason {
		t.Fatalf("reply = %+v, want reason %q", reply, reason)
	}
}

func wantAddress(t *testing.T, reply *proto.EnrollResponse) string {
	t.Helper()
	if reply == nil || reply.Error != "" || reply.Address == "" {
		t.Fatalf("reply = %+v, want an address", reply)
	}
	return reply.Address
}

func TestANewDeviceWaitsAndIsNotPutIntoTheTunnel(t *testing.T) {
	r := approvalRig(t)
	wantReason(t, r.ask(t, 1, 0x11, "Xiaomi Redmi"), proto.EnrollPending)
	if r.tunnel.has(0x11) || r.reg.Len() != 0 {
		t.Fatal("a waiting device was let in")
	}
	o := r.srv.Overview(r.now)
	if len(o.Pending) != 1 || o.Pending[0].Name != "Xiaomi Redmi" || o.Pending[0].Notified || o.Pending[0].KeyChange {
		t.Fatalf("pending = %+v", o.Pending)
	}

	// Asking again keeps one request, not two.
	r.now = r.now.Add(10 * time.Second)
	wantReason(t, r.ask(t, 1, 0x11, "Xiaomi Redmi"), proto.EnrollPending)
	if got := len(r.srv.Overview(r.now).Pending); got != 1 {
		t.Fatalf("%d requests after asking twice", got)
	}
}

func TestAnApprovedDeviceGetsItsAddressOnTheNextAsk(t *testing.T) {
	r := approvalRig(t)
	wantReason(t, r.ask(t, 1, 0x11, "phone"), proto.EnrollPending)

	d, err := r.srv.Approve(idString(1), r.now)
	if err != nil {
		t.Fatal(err)
	}
	if !r.tunnel.has(0x11) {
		t.Fatal("approval did not put the key into the tunnel")
	}
	if got := wantAddress(t, r.ask(t, 1, 0x11, "phone")); got != d.Address {
		t.Fatalf("address %s, approval gave %s", got, d.Address)
	}
	if o := r.srv.Overview(r.now); len(o.Pending) != 0 || len(o.Devices) != 1 {
		t.Fatalf("overview after approval = %+v", o)
	}
}

func TestADeviceApprovedBeforeApprovalExistedKeepsWorking(t *testing.T) {
	r := newRig(t, Options{})
	addr := wantAddress(t, r.ask(t, 1, 0x11, "windows"))

	r.srv.Access = NewAccess(filepath.Join(t.TempDir(), "access.json"), AccessOptions{})
	r.srv.Approval = true
	if got := wantAddress(t, r.ask(t, 1, 0x11, "windows")); got != addr {
		t.Fatalf("address changed from %s to %s", addr, got)
	}
}

func TestAKeyChangeWaitsAndTheOldKeyKeepsWorkingUntilDecided(t *testing.T) {
	r := approvalRig(t)
	r.srv.Approval = false
	addr := wantAddress(t, r.ask(t, 1, 0x11, "phone"))
	r.srv.Approval = true

	wantReason(t, r.ask(t, 1, 0x22, "phone"), proto.EnrollPending)
	if !r.tunnel.has(0x11) || r.tunnel.has(0x22) {
		t.Fatal("a key change took effect before it was approved")
	}
	p := r.srv.Overview(r.now).Pending
	if len(p) != 1 || !p[0].KeyChange {
		t.Fatalf("pending = %+v, want one key change", p)
	}

	d, err := r.srv.Approve(idString(1), r.now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Address != addr {
		t.Errorf("approved key change moved the device from %s to %s", addr, d.Address)
	}
	if r.tunnel.has(0x11) || !r.tunnel.has(0x22) {
		t.Fatal("after approval the new key should replace the old one")
	}
}

func TestARefusedKeyChangeLeavesTheDeviceApproved(t *testing.T) {
	r := approvalRig(t)
	r.srv.Approval = false
	wantAddress(t, r.ask(t, 1, 0x11, "phone"))
	r.srv.Approval = true

	wantReason(t, r.ask(t, 1, 0x22, "phone"), proto.EnrollPending)
	if _, err := r.srv.Deny(idString(1), r.now); err != nil {
		t.Fatal(err)
	}
	wantReason(t, r.ask(t, 1, 0x22, "phone"), proto.EnrollDenied)
	wantAddress(t, r.ask(t, 1, 0x11, "phone"))
}

func TestARefusedDeviceIsTurnedAwayWithoutANewRequest(t *testing.T) {
	r := approvalRig(t)
	wantReason(t, r.ask(t, 1, 0x11, "phone"), proto.EnrollPending)
	if _, err := r.srv.Deny(idString(1), r.now); err != nil {
		t.Fatal(err)
	}
	wantReason(t, r.ask(t, 1, 0x11, "phone"), proto.EnrollDenied)
	// A reinstalled app has a new key, but it is the same device.
	wantReason(t, r.ask(t, 1, 0x33, "phone"), proto.EnrollDenied)
	o := r.srv.Overview(r.now)
	if len(o.Pending) != 0 || len(o.Denied) != 1 {
		t.Fatalf("overview = %+v", o)
	}
	if o.Denied[0].PublicKey != keyString(0x33) {
		t.Error("the refusal should follow the key the device asks with now")
	}

	// Let in after all: with the key it uses now.
	if _, err := r.srv.Allow(idString(1), r.now); err != nil {
		t.Fatal(err)
	}
	wantAddress(t, r.ask(t, 1, 0x33, "phone"))
}

func TestARevokedDeviceLeavesTheTunnelAndCannotComeBack(t *testing.T) {
	r := approvalRig(t)
	wantReason(t, r.ask(t, 1, 0x11, "phone"), proto.EnrollPending)
	if _, err := r.srv.Approve(idString(1), r.now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.srv.Revoke(idString(1), r.now); err != nil {
		t.Fatal(err)
	}
	if r.tunnel.has(0x11) || r.reg.Len() != 0 {
		t.Fatal("a revoked device is still in")
	}
	wantReason(t, r.ask(t, 1, 0x11, "phone"), proto.EnrollDenied)
}

func TestRefusalsHoldEvenWithoutApproval(t *testing.T) {
	r := approvalRig(t)
	r.srv.Approval = false
	wantAddress(t, r.ask(t, 1, 0x11, "phone"))
	if _, err := r.srv.Revoke(idString(1), r.now); err != nil {
		t.Fatal(err)
	}
	wantReason(t, r.ask(t, 1, 0x11, "phone"), proto.EnrollDenied)
	// Everybody else still gets in without asking.
	wantAddress(t, r.ask(t, 2, 0x22, "other"))
}

func TestAForgottenDeviceMayAskAgain(t *testing.T) {
	r := approvalRig(t)
	wantReason(t, r.ask(t, 1, 0x11, "phone"), proto.EnrollPending)
	if _, err := r.srv.Deny(idString(1), r.now); err != nil {
		t.Fatal(err)
	}
	if err := r.srv.Forget(idString(1)); err != nil {
		t.Fatal(err)
	}
	wantReason(t, r.ask(t, 1, 0x11, "phone"), proto.EnrollPending)
}

func TestAPreapprovedKeySkipsTheQueue(t *testing.T) {
	r := approvalRig(t)
	r.srv.Preapproved = []string{keyString(0x55)}
	req := request(0, 0x55, "selftest", r.now)
	req.DeviceID = proto.Encode(SelfTestDeviceID())
	blob, err := req.Seal(testKey)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := proto.OpenEnrollResponse(testKey, string(r.srv.Handle(peerAddr, []byte(blob))), req.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	wantAddress(t, reply)
	o := r.srv.Overview(r.now)
	if len(o.Devices) != 0 || o.Service != 1 {
		t.Fatalf("the server's own check should be left out of the list: %+v", o)
	}

	// Anybody else claiming to be the check, with another key, waits.
	wantReason(t, r.ask(t, 9, 0x66, "selftest"), proto.EnrollPending)
}

func TestTheWaitingListIsCapped(t *testing.T) {
	r := approvalRig(t)
	r.srv.Access = NewAccess(filepath.Join(t.TempDir(), "a.json"), AccessOptions{MaxPending: 2})
	wantReason(t, r.ask(t, 1, 0x11, "a"), proto.EnrollPending)
	wantReason(t, r.ask(t, 2, 0x12, "b"), proto.EnrollPending)
	wantReason(t, r.ask(t, 3, 0x13, "c"), proto.EnrollBusy)
	// The ones already waiting are still answered.
	wantReason(t, r.ask(t, 1, 0x11, "a"), proto.EnrollPending)
}

func TestARequestExpiresAfterItStopsAsking(t *testing.T) {
	r := approvalRig(t)
	wantReason(t, r.ask(t, 1, 0x11, "a"), proto.EnrollPending)
	r.now = r.now.Add(DefaultPendingFor + time.Minute)
	if got := len(r.srv.Overview(r.now).Pending); got != 0 {
		t.Fatalf("%d requests after a week of silence", got)
	}
}

func TestTheOperatorIsToldOnceAndAgainOnlyForANewKey(t *testing.T) {
	r := approvalRig(t)
	wantReason(t, r.ask(t, 1, 0x11, "a"), proto.EnrollPending)
	if err := r.srv.MarkNotified([]string{idString(1)}); err != nil {
		t.Fatal(err)
	}
	wantReason(t, r.ask(t, 1, 0x11, "a"), proto.EnrollPending)
	if !r.srv.Overview(r.now).Pending[0].Notified {
		t.Fatal("asking again should not make the request new")
	}
	// A new key means a new code to match, so the operator must hear again.
	wantReason(t, r.ask(t, 1, 0x12, "a"), proto.EnrollPending)
	if r.srv.Overview(r.now).Pending[0].Notified {
		t.Fatal("a request with a new key should be told about again")
	}
}

func TestDecisionsOnUnknownDevicesSaySo(t *testing.T) {
	r := approvalRig(t)
	if _, err := r.srv.Approve(idString(1), r.now); !errors.Is(err, ErrNoRequest) {
		t.Errorf("approve: %v", err)
	}
	if _, err := r.srv.Deny(idString(1), r.now); !errors.Is(err, ErrNoRequest) {
		t.Errorf("deny: %v", err)
	}
	if _, err := r.srv.Revoke(idString(1), r.now); !errors.Is(err, ErrUnknownDevice) {
		t.Errorf("revoke: %v", err)
	}
	if _, err := r.srv.Allow(idString(1), r.now); !errors.Is(err, ErrNoRefusal) {
		t.Errorf("allow: %v", err)
	}
	if err := r.srv.Forget(idString(1)); !errors.Is(err, ErrNoRefusal) {
		t.Errorf("forget: %v", err)
	}
}

func TestApprovingAKeyThatAnotherDeviceHoldsIsRefused(t *testing.T) {
	r := approvalRig(t)
	r.srv.Approval = false
	wantAddress(t, r.ask(t, 1, 0x11, "first"))
	r.srv.Approval = true
	wantReason(t, r.ask(t, 2, 0x11, "second"), proto.EnrollPending)
	_, err := r.srv.Approve(idString(2), r.now)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != proto.EnrollKeyInUse {
		t.Fatalf("err = %v, want key_in_use", err)
	}
}

func TestTheAccessListSurvivesARestart(t *testing.T) {
	r := approvalRig(t)
	path := filepath.Join(t.TempDir(), "access.json")
	r.srv.Access = NewAccess(path, AccessOptions{})
	wantReason(t, r.ask(t, 1, 0x11, "a"), proto.EnrollPending)
	wantReason(t, r.ask(t, 2, 0x22, "b"), proto.EnrollPending)
	if _, err := r.srv.Deny(idString(2), r.now); err != nil {
		t.Fatal(err)
	}

	again, err := LoadAccess(path, AccessOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if p, d := again.Pending(r.now), again.Denied(); len(p) != 1 || p[0].ID != idString(1) || len(d) != 1 || d[0].ID != idString(2) {
		t.Fatalf("after reload: pending %+v, denied %+v", p, d)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseAccess(raw); err != nil {
		t.Fatalf("the saved file does not check out: %v", err)
	}
}

func TestABrokenAccessListIsAnError(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":   "{",
		"version":    `{"version":2,"pending":[],"denied":[]}`,
		"bad id":     `{"version":1,"pending":[{"id":"x","pub":"` + keyString(1) + `"}],"denied":[]}`,
		"repeat id":  `{"version":1,"pending":[],"denied":[{"id":"` + idString(1) + `","pub":"` + keyString(1) + `"},{"id":"` + idString(1) + `","pub":"` + keyString(2) + `"}]}`,
		"bad key":    `{"version":1,"pending":[],"denied":[{"id":"` + idString(1) + `","pub":"short"}]}`,
		"repeat req": `{"version":1,"pending":[{"id":"` + idString(1) + `","pub":"` + keyString(1) + `"},{"id":"` + idString(1) + `","pub":"` + keyString(2) + `"}],"denied":[]}`,
	} {
		if _, _, err := ParseAccess([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
