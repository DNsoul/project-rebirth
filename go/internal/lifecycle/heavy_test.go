package lifecycle

import (
	"testing"
	"time"

	"awgrotator/internal/node"
	"awgrotator/internal/usage"
)

func heavyReport() *node.UsageView {
	return &node.UsageView{
		Report: usage.Report{
			Period: usage.Day,
			Total:  usage.Bytes{Down: 190e9, Up: 2e9},
			Devices: []usage.Item{
				{Key: "pc", Bytes: usage.Bytes{Down: 154e9, Up: 30e9}},
				{Key: "phone", Bytes: usage.Bytes{Down: 8e9}},
			},
		},
		Names: map[string]node.DeviceLabel{"pc": {Name: "DESKTOP-UE0P2DK", Code: "0235"}},
	}
}

func TestADeviceThatUsesALotIsToldOncePerDayInTheDaytime(t *testing.T) {
	r := newAccountRig(t, unpaid())
	r.host.report = heavyReport()
	look := func(at time.Time) {
		r.clock.Set(at)
		r.m.maybeHeavy(t.Context(), at)
	}
	heavy := func() []HeavyUsage {
		var out []HeavyUsage
		for _, e := range *r.events {
			if e.Kind == EventHeavy && e.Heavy != nil {
				out = append(out, *e.Heavy)
			}
		}
		return out
	}

	look(time.Date(2027, 3, 1, 3, 0, 0, 0, msk)) // night: waits
	if n := len(heavy()); n != 0 {
		t.Fatalf("told at night")
	}
	look(time.Date(2027, 3, 1, 10, 0, 0, 0, msk))
	look(time.Date(2027, 3, 1, 10, 10, 0, 0, msk)) // too soon to ask again
	look(time.Date(2027, 3, 1, 15, 0, 0, 0, msk))  // asked again, already told
	r.restart()
	look(time.Date(2027, 3, 1, 16, 0, 0, 0, msk))
	got := heavy()
	if len(got) != 1 {
		t.Fatalf("told %d times on one day, want once: %+v", len(got), got)
	}
	want := HeavyUsage{ID: "pc", Name: "DESKTOP-UE0P2DK", Code: "0235", Bytes: 184e9, Total: 192e9}
	if got[0] != want {
		t.Errorf("told %+v, want %+v", got[0], want)
	}

	look(time.Date(2027, 3, 2, 10, 0, 0, 0, msk)) // the next day, still heavy
	if n := len(heavy()); n != 2 {
		t.Fatalf("the next day: told %d times in all, want 2", n)
	}

	r2 := newAccountRig(t, unpaid())
	r2.host.report = heavyReport()
	r2.m.current = false
	r2.m.maybeHeavy(t.Context(), time.Date(2027, 3, 1, 12, 0, 0, 0, msk))
	if len(*r2.events) != 0 {
		t.Error("a server that has handed over spoke")
	}
}

func TestTheSuccessorCarriesOnCountingTraffic(t *testing.T) {
	store, err := usage.Open(t.TempDir() + "/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	store.Add(t0, []usage.Record{{Device: "pc", Site: "YouTube", Bytes: usage.Bytes{Down: 5}}})
	raw, err := store.Snapshot(t0)
	if err != nil {
		t.Fatal(err)
	}

	r := newRig(t)
	r.host.usage = raw
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})
	got, ok := filesByPath(r)[node.DefaultUsagePath]
	if !ok || string(got) != string(raw) {
		t.Fatalf("the traffic history was not passed on; files: %v", paths(r.prov.files))
	}
	for _, f := range r.prov.files {
		if f.Path == node.DefaultUsagePath && f.Mode != "0600" {
			t.Errorf("mode %s", f.Mode)
		}
	}

	bad := newRig(t)
	bad.host.usage = []byte(`{"version":7}`)
	mustMove(t, bad, MoveRequest{Trigger: TriggerCommand})
	if _, ok := filesByPath(bad)[node.DefaultUsagePath]; ok {
		t.Error("a history that does not check out was passed on")
	}
}

func TestTheSuccessorKeepsTheSitesSentDirect(t *testing.T) {
	raw := []byte("# Сайты\nsteamcontent.com\nriotcdn.net\n")
	r := newRig(t)
	r.host.direct = raw
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})
	got, ok := filesByPath(r)[node.DefaultDirectPath]
	if !ok || string(got) != string(raw) {
		t.Fatalf("the list was not passed on; files: %v", paths(r.prov.files))
	}

	none := newRig(t)
	mustMove(t, none, MoveRequest{Trigger: TriggerCommand})
	if _, ok := filesByPath(none)[node.DefaultDirectPath]; ok {
		t.Error("an empty list was passed on")
	}
}
