package lifecycle

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"awgrotator/internal/globalping"
	"awgrotator/internal/node"
)

// The outside check asks probes in Russia whether they can reach the server.
//
// Which probes matters more than how many. Servers have died for the people
// using them while probes in Russian datacenters still reached them: blocking
// is done by the providers people use at home and on the phone, and datacenter
// networks are not behind the same filters. So the verdict rests on probes in
// home and mobile networks; the datacenter ones are asked too and written down,
// so that the record can show later which of the two told the truth.
//
// And which traffic matters: the tunnel is UDP. Each probe is asked twice, by
// ping and by a UDP question to a port next to the tunnel's that the server
// itself answers. A probe counts as reaching the server only if both got
// through.

// UDPTarget is where the server answers the outside check over UDP: a port next
// to the tunnel's and a name only this server answers to, with the address the
// answer carries.
type UDPTarget struct {
	Port int
	Name string
	Want string
}

// ExternalDetail is what one outside check found.
type ExternalDetail struct {
	// Target and Control are what the verdict is made of: probes in home and
	// mobile networks in the target country, and probes abroad, each counted
	// as reaching the server only when every kind of traffic asked got
	// through.
	Target, Control Reach

	// Home, Datacenter and Abroad break it down per kind of traffic. Only ICMP
	// and UDP are used.
	Home, Datacenter, Abroad Tally

	// UDP says whether the UDP question was asked. It is not when the server
	// has no responder (an older node) or the question could not be sent.
	UDP bool

	// Datacenters says the verdict had to lean on datacenter probes because too
	// few home ones were available.
	Datacenters bool

	// Failed names the home networks that did not reach the server, as
	// "AS12389 Kostroma: udp".
	Failed []string

	// Note says why something expected is missing, such as the UDP question.
	Note string
}

// DetailedProber is a Prober that can also say how each kind of network and
// each kind of traffic came out.
type DetailedProber interface {
	Prober
	ReachDetail(ctx context.Context, ip string) (ExternalDetail, error)
}

// How many probes of each kind one check asks for. Every probe of every
// measurement counts against the free allowance of 250 an hour per address:
// two measurements of 16 probes every ten minutes use 192 of it.
const (
	homeProbes       = 9
	datacenterProbes = 3
	controlProbes    = 4

	// minHomeProbes is how many home probes the verdict needs. With fewer, the
	// datacenter ones are counted in with them.
	minHomeProbes = 3
)

// hostingASNs are networks whose probes Globalping tags as home connections
// although the networks belong to hosting companies. They are counted with the
// datacenters.
var hostingASNs = map[int]bool{
	9123:   true, // Timeweb
	16262:  true, // Datacheap
	48282:  true, // Hosting technology (VDSina)
	207957: true, // SERV.HOST
	209805: true, // SBCLOUD
	216334: true, // New Hosting Technologies
}

// GlobalpingProber measures reachability from probes in the target country and
// from a control group elsewhere.
type GlobalpingProber struct {
	Client *globalping.Client

	// Country is where the server needs to be reachable from, and Control a
	// country far from it. The defaults are Russia and Germany.
	Country string
	Control string

	// UDP says where the server answers over UDP, or false when it does not.
	// Without it the check is ping alone.
	UDP func(ctx context.Context) (UDPTarget, bool)
}

// NodeUDPTarget reads where the node answers the outside check from the
// node's status, for GlobalpingProber.UDP.
func NodeUDPTarget(host Host) func(ctx context.Context) (UDPTarget, bool) {
	return func(ctx context.Context) (UDPTarget, bool) {
		st, err := host.NodeStatus(ctx)
		if err != nil || st.Reach == nil {
			return UDPTarget{}, false
		}
		return UDPTarget{Port: st.Reach.Port, Name: st.Reach.Name, Want: node.ReachAnswer}, true
	}
}

// Reach implements Prober.
func (p *GlobalpingProber) Reach(ctx context.Context, ip string) (target, control Reach, err error) {
	d, err := p.ReachDetail(ctx, ip)
	if err != nil {
		return Reach{}, Reach{}, err
	}
	return d.Target, d.Control, nil
}

// ReachDetail implements DetailedProber.
func (p *GlobalpingProber) ReachDetail(ctx context.Context, ip string) (ExternalDetail, error) {
	gp := p.Client
	if gp == nil {
		gp = globalping.New()
	}
	country := orDefault(p.Country, "RU")

	ping, err := gp.PingFrom(ctx, ip, []globalping.Location{
		{Country: country, Tags: []string{globalping.TagEyeball}, Limit: homeProbes},
		{Country: country, Tags: []string{globalping.TagDatacenter}, Limit: datacenterProbes},
		{Country: orDefault(p.Control, "DE"), Limit: controlProbes},
	})
	if err != nil {
		return ExternalDetail{}, err
	}

	var udp []globalping.Outcome
	note := ""
	if p.UDP != nil {
		if t, ok := p.UDP(ctx); ok {
			res, err := gp.AskSameProbes(ctx, ping.ID, ip, t.Port, t.Name, t.Want)
			if err != nil {
				note = "UDP не проверен: " + briefError(err)
			} else {
				udp = pairOutcomes(ping.Outcomes, res.Outcomes)
			}
		} else {
			note = "UDP не проверен: сервер не отвечает на проверку по UDP"
		}
	}
	d := judgeExternal(country, ping.Outcomes, udp)
	d.Note = note
	return d, nil
}

// pairOutcomes lines the UDP answers up with the ping ones. Globalping returns
// the probes of a repeated measurement in the same order, but a probe that went
// away in between leaves a gap, so they are matched by who the probe is. A ping
// outcome with no partner gets a failed UDP outcome: the UDP question never got
// an answer from that place.
func pairOutcomes(ping, udp []globalping.Outcome) []globalping.Outcome {
	out := make([]globalping.Outcome, len(ping))
	used := make([]bool, len(udp))
	for i, p := range ping {
		out[i] = globalping.Outcome{Probe: p.Probe, Detail: "проба не ответила"}
		if i < len(udp) && !used[i] && sameProbe(p.Probe, udp[i].Probe) {
			out[i], used[i] = udp[i], true
			continue
		}
		for j, u := range udp {
			if !used[j] && sameProbe(p.Probe, u.Probe) {
				out[i], used[j] = u, true
				break
			}
		}
	}
	return out
}

func sameProbe(a, b globalping.Probe) bool {
	return a.ASN == b.ASN && a.City == b.City && a.Country == b.Country && a.Network == b.Network
}

// probeGroup says which group a probe belongs to.
func probeGroup(country string, p globalping.Probe) string {
	switch {
	case !strings.EqualFold(p.Country, country):
		return "abroad"
	case p.HasTag(globalping.TagEyeball) && !hostingASNs[p.ASN]:
		return "home"
	}
	return "datacenter"
}

// judgeExternal adds up the outcomes. udp is nil when the UDP question was not
// asked; otherwise it is lined up with ping.
func judgeExternal(country string, ping, udp []globalping.Outcome) ExternalDetail {
	d := ExternalDetail{UDP: udp != nil}
	var home, dc, abroad Reach
	for i, o := range ping {
		group := probeGroup(country, o.Probe)
		tally := map[string]*Tally{"home": &d.Home, "datacenter": &d.Datacenter, "abroad": &d.Abroad}[group]
		both := map[string]*Reach{"home": &home, "datacenter": &dc, "abroad": &abroad}[group]

		ok := o.OK
		count(&tally.ICMP, o.OK)
		var failed []string
		if !o.OK {
			failed = append(failed, "icmp")
		}
		if udp != nil {
			count(&tally.UDP, udp[i].OK)
			ok = ok && udp[i].OK
			if !udp[i].OK {
				failed = append(failed, "udp")
			}
		}
		both.Total++
		if ok {
			both.Reached++
		}
		if group == "home" && len(failed) > 0 {
			d.Failed = append(d.Failed, fmt.Sprintf("AS%d %s: %s", o.Probe.ASN, o.Probe.City, strings.Join(failed, "+")))
		}
	}
	sort.Strings(d.Failed)

	d.Target, d.Control = home, abroad
	if home.Total < minHomeProbes {
		d.Target = Reach{Reached: home.Reached + dc.Reached, Total: home.Total + dc.Total}
		d.Datacenters = true
	}
	return d
}

func count(c *[2]int, ok bool) {
	c[1]++
	if ok {
		c[0]++
	}
}
