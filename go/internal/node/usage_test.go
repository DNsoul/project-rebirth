package node

import (
	"path/filepath"
	"strings"
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

func TestTheOperatorCanNameADeviceAndTheNameIsUsedEverywhere(t *testing.T) {
	ctl, srv, now := controlRig(t)
	id := proto.Encode(fill(1, proto.DeviceIDSize))
	if resp := ctl.Handle(ControlRequest{Op: OpApprove, ID: id}); !resp.OK {
		t.Fatalf("approve: %+v", resp)
	}
	resp := ctl.Handle(ControlRequest{Op: OpRename, ID: id, Label: "  Ноутбук Пети "})
	if !resp.OK || resp.Name != "Ноутбук Пети" {
		t.Fatalf("rename: %+v", resp)
	}
	o := ctl.Handle(ControlRequest{Op: OpOverview}).Overview
	if d := o.Devices[0]; d.Label != "Ноутбук Пети" || d.Name != "Xiaomi Redmi Note 12" || d.Shown() != "Ноутбук Пети" {
		t.Errorf("device = %+v", d.Device)
	}
	store, _ := usage.Open(filepath.Join(t.TempDir(), "usage.json"))
	store.Add(*now, []usage.Record{{Device: id, Site: "YouTube", Bytes: usage.Bytes{Down: 1}}})
	ctl.Usage = store
	if u := ctl.Handle(ControlRequest{Op: OpUsage, Period: usage.Day}).Usage; u.Names[id].Name != "Ноутбук Пети" {
		t.Errorf("traffic report names it %q", u.Names[id].Name)
	}

	// The name stays in the saved list.
	if d, ok := srv.Registry.Lookup(id); !ok || d.Label != "Ноутбук Пети" {
		t.Errorf("registry = %+v", d)
	}

	for _, bad := range []string{strings.Repeat("я", 33), "bad\x07name"} {
		if r := ctl.Handle(ControlRequest{Op: OpRename, ID: id, Label: bad}); r.OK {
			t.Errorf("%q was taken", bad)
		}
	}
	if r := ctl.Handle(ControlRequest{Op: OpRename, ID: "nobody", Label: "x"}); r.OK || !r.Missing {
		t.Errorf("unknown device: %+v", r)
	}
	if r := ctl.Handle(ControlRequest{Op: OpRename, ID: id, Label: ""}); !r.OK || r.Name != "Xiaomi Redmi Note 12" {
		t.Errorf("clearing the name: %+v", r)
	}
}
