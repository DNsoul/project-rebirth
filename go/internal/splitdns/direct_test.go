package splitdns

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestOnlyASiteCanBeSentDirect(t *testing.T) {
	for in, want := range map[string]string{
		"steamcontent.com":             "steamcontent.com",
		"Cache1.SteamContent.com.":     "cache1.steamcontent.com",
		"*.riotcdn.net":                "riotcdn.net",
		"dgeft87wbj63p.cloudfront.net": "dgeft87wbj63p.cloudfront.net",
		"steampipe.akamaized.net":      "steampipe.akamaized.net",
		"пример.рф":                    "xn--e1afmkfd.xn--p1ai",
		"example.co.uk":                "example.co.uk",
	} {
		got, err := CheckDirectName(in)
		if err != nil || got != want {
			t.Errorf("CheckDirectName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]error{
		"com":            ErrTooBroad,
		"co.uk":          ErrTooBroad,
		"cloudfront.net": ErrTooBroad,
		"github.io":      ErrTooBroad,
		"localhost":      ErrTooBroad,
		"":               ErrNotAName,
		"http://x.com/":  ErrNotAName,
		"a b.com":        ErrNotAName,
	} {
		if _, err := CheckDirectName(in); !errors.Is(err, want) {
			t.Errorf("CheckDirectName(%q) = %v, want %v", in, err, want)
		}
	}
}

func TestTheDirectListKeepsOrderAndSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "direct.txt")
	l, err := OpenDirectList(path)
	if err != nil || len(l.Names()) != 0 {
		t.Fatalf("a missing file: %v, %v", l.Names(), err)
	}
	for _, n := range []string{"steamcontent.com", "Riotcdn.net", "steamcontent.com"} {
		if _, err := l.Add(n); err != nil {
			t.Fatal(err)
		}
	}
	if got := l.Names(); !slices.Equal(got, []string{"steamcontent.com", "riotcdn.net"}) {
		t.Errorf("names = %v", got)
	}
	if _, err := l.Add("com"); !errors.Is(err, ErrTooBroad) {
		t.Errorf("adding com: %v", err)
	}

	again, err := OpenDirectList(path)
	if err != nil || !slices.Equal(again.Names(), l.Names()) {
		t.Fatalf("reopened: %v, %v", again.Names(), err)
	}
	if !again.Rules().Match("cache7-mow1.steamcontent.com") || again.Rules().Match("steampowered.com") {
		t.Error("the rules do not follow the list")
	}

	if err := again.Remove("STEAMCONTENT.com"); err != nil {
		t.Fatal(err)
	}
	if err := again.Remove("steamcontent.com"); !errors.Is(err, ErrNotListed) {
		t.Errorf("removing twice: %v", err)
	}
	third, _ := OpenDirectList(path)
	if !slices.Equal(third.Names(), []string{"riotcdn.net"}) {
		t.Errorf("after removal: %v", third.Names())
	}
}

func TestADirectListWithAStaleNameKeepsTheRest(t *testing.T) {
	raw := []byte("# comment\nsteamcontent.com\ncloudfront.net\n\nriotcdn.net # games\nsteamcontent.com\n")
	names, err := ParseDirectList(raw)
	if err != nil || !slices.Equal(names, []string{"steamcontent.com", "riotcdn.net"}) {
		t.Errorf("names = %v, %v", names, err)
	}
}

func TestTheDirectListHasALimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct.txt")
	l, _ := OpenDirectList(path)
	l.names = make([]string, MaxDirect)
	if _, err := l.Add("one-more.com"); !errors.Is(err, ErrDirectFull) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused name was saved")
	}
}

func TestTheOperatorsChoiceSendsASiteDirectButTunnelStillWins(t *testing.T) {
	d := &Decider{RU: localSet(t), ForceTunnel: mustRules(t, "mc.yandex.ru")}
	foreign := []netip.Addr{netip.MustParseAddr("142.250.1.1")}
	if d.Decide("cache1.steamcontent.com", foreign).Verdict != Tunnel {
		t.Fatal("a foreign site went direct with no rule")
	}
	d.Chosen.Store(mustRules(t, "steamcontent.com", "yandex.ru"))
	if got := d.Decide("cache1.steamcontent.com", foreign); got.Verdict != Direct {
		t.Errorf("chosen site: %+v", got)
	}
	if got := d.Decide("mc.yandex.ru", foreign); got.Verdict != Tunnel {
		t.Errorf("a tracker under a chosen site: %+v", got)
	}
}

func TestAChangedRuleTakesEffectWithoutWaitingForTheCache(t *testing.T) {
	r := newRig(t)
	r.both("cache1.steamcontent.com", "142.250.1.1")
	if got := answerAddrs(r.ask(t, "cache1.steamcontent.com", dnsmessage.TypeA)); len(got) != 1 || !inStandInRange(got[0]) {
		t.Fatalf("before: %v", got)
	}

	r.srv.cfg.Decider.Chosen.Store(mustRules(t, "steamcontent.com"))
	if got := answerAddrs(r.ask(t, "cache1.steamcontent.com", dnsmessage.TypeA)); len(got) != 1 || !inStandInRange(got[0]) {
		t.Fatalf("the cached decision should hold until forgotten: %v", got)
	}
	r.srv.Forget()
	if got := answerAddrs(r.ask(t, "cache1.steamcontent.com", dnsmessage.TypeA)); len(got) != 1 || got[0] != "142.250.1.1" {
		t.Errorf("after: %v, want the real address", got)
	}
}
