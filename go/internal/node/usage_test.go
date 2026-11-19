package node

import (
	"path/filepath"
	"testing"

	"awgrotator/internal/proto"
	"awgrotator/internal/usage"
)

func TestTheTrafficReportNamesDevicesAndRefusesWithoutAHistory(t *testing.T) {
	ctl, _, now := controlRig(t)
	if resp := ctl.Handle(ControlRequest{Op: OpUsage, Period: usage.Day}); resp.OK || resp.Error == "" {
		t.Fatalf("without a history: %+v", resp)
	}

	id := proto.Encode(fill(1, proto.DeviceIDSize))
	if resp := ctl.Handle(ControlRequest{Op: OpApprove, ID: id}); !resp.OK {
		t.Fatalf("approve: %+v", resp)
	}
	store, err := usage.Open(filepath.Join(t.TempDir(), "usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.Add(*now, []usage.Record{
		{Device: id, Site: "YouTube", Bytes: usage.Bytes{Down: 900}},
		{Device: "gone", Site: "Steam", Bytes: usage.Bytes{Down: 100}},
	})
	ctl.Usage = store

	resp := ctl.Handle(ControlRequest{Op: OpUsage, Period: usage.Day})
	if !resp.OK || resp.Usage == nil {
		t.Fatalf("resp = %+v", resp)
	}
	u := resp.Usage
	if u.Total.Total() != 1000 || len(u.Devices) != 2 || u.Devices[0].Key != id || u.Sites[0].Key != "YouTube" {
		t.Errorf("report = %+v", u.Report)
	}
	if l := u.Names[id]; l.Name != "Xiaomi Redmi Note 12" || l.Code != proto.ApprovalCode(fill(0x11, proto.TunnelKeySize)) {
		t.Errorf("label = %+v", l)
	}
	if _, ok := u.Names["gone"]; ok {
		t.Error("a removed device has a name")
	}

	one := ctl.Handle(ControlRequest{Op: OpUsage, Period: usage.Week, ID: id})
	if !one.OK || one.Usage.Total.Total() != 900 || len(one.Usage.Devices) != 0 {
		t.Errorf("one device = %+v", one)
	}
	if bad := ctl.Handle(ControlRequest{Op: OpUsage, Period: "year"}); bad.OK {
		t.Error("an unknown period was accepted")
	}
}
