package deploy

import (
	"net/netip"
	"strings"
	"testing"
)

var stand = netip.MustParsePrefix("198.18.0.0/15")

const goodCheck = `Q github.com 198.18.0.2
Q yandex.ru 5.255.255.77 77.88.44.55
Q vk.com 87.240.129.133 87.240.132.67
Q ozon.ru 185.73.193.68
Q gosuslugi.ru
MAP
table ip awgsplit {
	map fake2real {
		type ipv4_addr : ipv4_addr
		elements = { 198.18.0.2 : 140.82.121.3,
			     198.18.0.3 : 1.1.1.1 }
	}
}
`

func TestParseSplitCheck(t *testing.T) {
	rep, err := ParseSplitCheck(goodCheck, stand)
	if err != nil {
		t.Fatalf("ParseSplitCheck: %v", err)
	}
	if len(rep.Probes) != 5 {
		t.Fatalf("%d probes, want 5", len(rep.Probes))
	}

	byName := map[string]SplitProbe{}
	for _, p := range rep.Probes {
		byName[p.Domain] = p
	}
	if p := byName["github.com"]; !p.Tunnelled || len(p.Addrs) != 1 {
		t.Errorf("github.com = %+v, want one stand-in", p)
	}
	if p := byName["yandex.ru"]; p.Tunnelled || len(p.Addrs) != 2 {
		t.Errorf("yandex.ru = %+v, want two real addresses", p)
	}
	if p := byName["gosuslugi.ru"]; p.Tunnelled || len(p.Addrs) != 0 {
		t.Errorf("an unanswered query = %+v, want no addresses and not tunnelled", p)
	}
	if rep.MapEntries != 2 {
		t.Errorf("MapEntries = %d, want 2", rep.MapEntries)
	}
}

// One real address among stand-ins means the site is not wholly tunnelled.
func TestMixedAnswerIsNotTunnelled(t *testing.T) {
	rep, err := ParseSplitCheck("Q a.example 198.18.0.2 8.8.8.8 \nMAP\n", stand)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Probes[0].Tunnelled {
		t.Error("an answer mixing a stand-in with a real address was reported as tunnelled")
	}
}

func TestParseSplitCheckWithNothingIsAnError(t *testing.T) {
	if _, err := ParseSplitCheck("", stand); err == nil {
		t.Error("an empty output was accepted")
	}
	if _, err := ParseSplitCheck("bash: dig: command not found\n", stand); err == nil {
		t.Error("an output with no answers was accepted")
	}
}

func TestParseSplitCheckIgnoresNoiseInTheAnswer(t *testing.T) {
	// dig prints comments and warnings on failure; only addresses count.
	rep, err := ParseSplitCheck("Q a.example ;; connection timed out; no servers could be reached \nMAP\n", stand)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Probes[0].Addrs) != 0 {
		t.Errorf("noise was read as addresses: %v", rep.Probes[0].Addrs)
	}
}

func TestReportStringIsReadable(t *testing.T) {
	rep, err := ParseSplitCheck(goodCheck, stand)
	if err != nil {
		t.Fatal(err)
	}
	out := rep.String()
	for _, want := range []string{"github.com", "туннель", "напрямую", "нет ответа", "переводов в ядре: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report is missing %q:\n%s", want, out)
		}
	}
}

func TestCheckScriptAsksEveryProbeThroughTheResolver(t *testing.T) {
	script := checkScript("198.18.0.1")
	for _, d := range append([]string{foreignProbe}, localProbes...) {
		if !strings.Contains(script, "@198.18.0.1 "+d) {
			t.Errorf("the script does not ask %s through the resolver", d)
		}
	}
	if !strings.Contains(script, "nft list map ip awgsplit fake2real") {
		t.Error("the script does not read the translation map")
	}
}

// An empty map still prints its declaration, which contains " : ". Counting
// separators would have reported a translation that is not there.
func TestAnEmptyMapHasNoEntries(t *testing.T) {
	out := `Q github.com 198.18.0.2 
MAP
table ip awgsplit {
	map fake2real {
		type ipv4_addr : ipv4_addr
	}
}
`
	rep, err := ParseSplitCheck(out, stand)
	if err != nil {
		t.Fatal(err)
	}
	if rep.MapEntries != 0 {
		t.Errorf("MapEntries = %d for an empty map, want 0", rep.MapEntries)
	}
}
