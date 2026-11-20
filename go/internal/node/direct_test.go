package node

import (
	"net/netip"
	"path/filepath"
	"slices"
	"testing"

	"awgrotator/internal/splitdns"
)

func directRig(t *testing.T) (*Controller, *splitdns.Decider, *int) {
	t.Helper()
	ctl, _, _ := controlRig(t)
	list, err := splitdns.OpenDirectList(filepath.Join(t.TempDir(), "direct.txt"))
	if err != nil {
		t.Fatal(err)
	}
	tracker, _ := splitdns.NewRules("mc.yandex.ru")
	d := &splitdns.Decider{ForceTunnel: tracker}
	forgot := 0
	ctl.Direct = NewDirectRouting(list, d, func() { forgot++ })
	return ctl, d, &forgot
}

func TestTheOperatorsListChangesRoutingAtOnce(t *testing.T) {
	ctl, d, forgot := directRig(t)
	foreign := []netip.Addr{netip.MustParseAddr("142.250.1.1")}
	*forgot = 0

	resp := ctl.Handle(ControlRequest{Op: OpDirectAdd, Name: "SteamContent.com"})
	if !resp.OK || resp.Name != "steamcontent.com" || !slices.Equal(resp.Direct, []string{"steamcontent.com"}) {
		t.Fatalf("add: %+v", resp)
	}
	if d.Decide("cache1.steamcontent.com", foreign).Verdict != splitdns.Direct || *forgot != 1 {
		t.Errorf("the decider did not take the list (forgot %d)", *forgot)
	}

	list := ctl.Handle(ControlRequest{Op: OpDirect})
	if !list.OK || !slices.Equal(list.Direct, []string{"steamcontent.com"}) {
		t.Errorf("list: %+v", list)
	}

	if resp := ctl.Handle(ControlRequest{Op: OpDirectRemove, Name: "steamcontent.com"}); !resp.OK || len(resp.Direct) != 0 {
		t.Errorf("remove: %+v", resp)
	}
	if d.Decide("cache1.steamcontent.com", foreign).Verdict != splitdns.Tunnel {
		t.Error("a removed site still goes direct")
	}
	if resp := ctl.Handle(ControlRequest{Op: OpDirectRemove, Name: "steamcontent.com"}); resp.OK || !resp.Missing {
		t.Errorf("removing twice: %+v", resp)
	}
}

func TestTheOperatorsListRefusesWithAReason(t *testing.T) {
	ctl, _, _ := directRig(t)
	for name, reason := range map[string]string{
		"com":            DirectTooBroad,
		"cloudfront.net": DirectTooBroad,
		"http://x.com":   DirectNotAName,
		"mc.yandex.ru":   DirectAlwaysTunnel,
	} {
		if resp := ctl.Handle(ControlRequest{Op: OpDirectAdd, Name: name}); resp.OK || resp.Reason != reason {
			t.Errorf("%q: %+v, want reason %q", name, resp, reason)
		}
	}
	// A site that only contains a tracker is fine: the tracker still tunnels.
	if resp := ctl.Handle(ControlRequest{Op: OpDirectAdd, Name: "yandex.ru"}); !resp.OK {
		t.Errorf("yandex.ru: %+v", resp)
	}
}

func TestWithoutSplitRoutingThereIsNoList(t *testing.T) {
	ctl, _, _ := controlRig(t)
	if resp := ctl.Handle(ControlRequest{Op: OpDirect}); resp.OK || resp.Error == "" {
		t.Errorf("resp = %+v", resp)
	}
}
