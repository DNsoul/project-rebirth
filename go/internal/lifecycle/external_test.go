package lifecycle

import (
	"context"
	"slices"
	"testing"
	"time"

	"awgrotator/internal/globalping"
	"awgrotator/internal/node"
)

func probe(country string, asn int, city string, tags ...string) globalping.Probe {
	return globalping.Probe{Country: country, ASN: asn, City: city, Network: "net" + city, Tags: tags}
}

func outcome(p globalping.Probe, ok bool) globalping.Outcome {
	return globalping.Outcome{Probe: p, OK: ok}
}

var (
	rostelecom = probe("RU", 12389, "Kostroma", globalping.TagEyeball)
	beeline    = probe("RU", 3216, "Ufa", globalping.TagEyeball)
	domru      = probe("RU", 52207, "Tula", globalping.TagEyeball)
	timeweb    = probe("RU", 9123, "Moscow", globalping.TagEyeball) // a hoster tagged as home
	selectel   = probe("RU", 49505, "Moscow", globalping.TagDatacenter)
	frankfurt  = probe("DE", 24940, "Frankfurt", globalping.TagDatacenter)
	berlin     = probe("DE", 3320, "Berlin", globalping.TagEyeball)
)

func TestTheVerdictRestsOnHomeNetworksAndBothKindsOfTraffic(t *testing.T) {
	ping := []globalping.Outcome{
		outcome(rostelecom, true), outcome(beeline, true), outcome(domru, true),
		outcome(timeweb, true), outcome(selectel, true),
		outcome(frankfurt, true), outcome(berlin, true),
	}
	udp := []globalping.Outcome{
		outcome(rostelecom, false), outcome(beeline, false), outcome(domru, true),
		outcome(timeweb, true), outcome(selectel, true),
		outcome(frankfurt, true), outcome(berlin, true),
	}
	d := judgeExternal("RU", ping, udp)
	if d.Target != (Reach{1, 3}) || d.Control != (Reach{2, 2}) || d.Datacenters || !d.UDP {
		t.Fatalf("%+v", d)
	}
	if d.Home.ICMP != [2]int{3, 3} || d.Home.UDP != [2]int{1, 3} {
		t.Errorf("home %+v", d.Home)
	}
	if d.Datacenter.ICMP != [2]int{2, 2} {
		t.Errorf("the hoster belongs with the datacenters: %+v", d.Datacenter)
	}
	if !slices.Equal(d.Failed, []string{"AS12389 Kostroma: udp", "AS3216 Ufa: udp"}) {
		t.Errorf("failed %v", d.Failed)
	}
	for i := 0; i < 3; i++ {
		udp[i].OK = false
	}
	if d := judgeExternal("RU", ping, udp); Classify(d.Target, d.Control) != Bad {
		t.Fatalf("home networks all cut off is a block: %+v", d)
	}
}

func TestTooFewHomeProbesLeanOnTheDatacenters(t *testing.T) {
	ping := []globalping.Outcome{outcome(rostelecom, false), outcome(selectel, true), outcome(timeweb, true), outcome(frankfurt, true)}
	d := judgeExternal("RU", ping, nil)
	if !d.Datacenters || d.Target != (Reach{2, 3}) || d.UDP {
		t.Fatalf("%+v", d)
	}
}

func TestUDPAnswersAreMatchedToTheirProbesEvenWithAGap(t *testing.T) {
	ping := []globalping.Outcome{outcome(rostelecom, true), outcome(beeline, true), outcome(domru, true)}
	udp := []globalping.Outcome{outcome(rostelecom, true), outcome(domru, true)}
	got := pairOutcomes(ping, udp)
	if !got[0].OK || got[1].OK || !got[2].OK || got[2].Probe.City != "Tula" {
		t.Fatalf("%+v", got)
	}
}

func TestTheUDPTargetComesFromTheNodesStatus(t *testing.T) {
	r := newRig(t)
	target := NodeUDPTarget(r.host)
	r.host.status = node.Status{UpdatedAt: r.clock.Now(), Reach: &node.ReachStatus{Port: 52886, Name: "abc.example.com"}}
	got, ok := target(context.Background())
	if !ok || got.Port != 52886 || got.Want != node.ReachAnswer {
		t.Fatalf("%+v %v", got, ok)
	}
}

func TestAClientKnockingWithoutGettingInIsRecorded(t *testing.T) {
	r := newRig(t)
	rev := &fakeReverse{report: report(true, true)}
	started := r.clock.Now().Add(-time.Hour)
	set := func(answered uint64, stuck, up int) {
		st := node.Status{UpdatedAt: r.clock.Now(), StartedAt: started,
			Handshakes: &node.HandshakeStats{Answered: answered, Stuck: stuck}}
		for i := 0; i < 2; i++ {
			p := node.PeerStatus{LastHandshake: r.clock.Now().Add(-time.Hour)}
			if i < up {
				p.LastHandshake = r.clock.Now()
			}
			st.Peers = append(st.Peers, p)
		}
		r.host.status = st
	}
	set(100, 0, 2)
	m, log := watched(t, r, rev)
	tick(t, m)
	for _, step := range []struct {
		answered  uint64
		stuck, up int
	}{{130, 0, 2}, {190, 2, 0}, {200, 2, 0}} {
		r.clock.Advance(2*time.Minute + time.Second)
		set(step.answered, step.stuck, step.up)
		tick(t, m)
	}
	got := samplesOf(t, log)
	if len(got) != 4 || got[0].Peers.Knocks != nil {
		t.Fatalf("%d lines, first %+v", len(got), got[0].Peers)
	}
	for i, w := range []int{30, 60, 10} {
		if k := got[i+1].Peers.Knocks; k == nil || *k != w {
			t.Errorf("line %d: knocks %v, want %d", i+1, k, w)
		}
	}
	var clients []string
	for _, c := range m.watch.changes {
		if c.What == "clients" {
			clients = append(clients, c.To)
		}
	}
	if !slices.Equal(clients, []string{"knocking"}) {
		t.Errorf("changes %v", clients)
	}
}
