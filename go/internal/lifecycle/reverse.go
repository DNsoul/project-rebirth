package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// The reverse probe asks the opposite question to the one the Globalping check
// asks. Globalping has hosts in Russia try to reach this server; the reverse
// probe has this server try to reach hosts in Russia. Any exchange needs a path
// in both directions, so if the server's address is cut off from Russia the
// answers do not come back, whichever direction the filter works in.
//
// It costs nothing, needs no third party, and can be asked often. It is run
// for the record only: the verdict it gives is written down and never acted on.

// Groups of checks.
const (
	GroupRU      = "ru"
	GroupControl = "control"
)

// Protocols a check uses.
const (
	ProtoTCP  = "tcp"
	ProtoUDP  = "udp"
	ProtoICMP = "icmp"
)

// Check is one try at one target over one protocol.
type Check struct {
	Group  string
	Proto  string
	Target string
	OK     bool
	Ms     int
	Err    string
}

// ReverseReport is everything one look found.
type ReverseReport struct{ Checks []Check }

// Count says how many checks of a group and protocol got an answer. An empty
// protocol counts them all.
func (r ReverseReport) Count(group, proto string) Reach {
	var out Reach
	for _, c := range r.Checks {
		if c.Group != group || (proto != "" && c.Proto != proto) {
			continue
		}
		out.Total++
		if c.OK {
			out.Reached++
		}
	}
	return out
}

// Round is how the look came out, judged the same way the Globalping check is:
// Russia against a control group abroad. Pings are left out of the verdict,
// because some perfectly healthy sites do not answer them; TCP and UDP count.
func (r ReverseReport) Round() Round {
	return Classify(r.stream(GroupRU), r.stream(GroupControl))
}

func (r ReverseReport) stream(group string) Reach {
	tcp, udp := r.Count(group, ProtoTCP), r.Count(group, ProtoUDP)
	return Reach{Reached: tcp.Reached + udp.Reached, Total: tcp.Total + udp.Total}
}

// Failed lists the Russian targets that did not answer over TCP or UDP, as
// "host/proto". Pings are left out here too.
func (r ReverseReport) Failed() []string {
	var out []string
	for _, c := range r.Checks {
		if c.Group == GroupRU && !c.OK && c.Proto != ProtoICMP {
			out = append(out, c.Target+"/"+c.Proto)
		}
	}
	return out
}

// MedianMs is the middle time of the TCP connections of a group that worked, or
// zero when none did. A path that is being throttled shows here before it breaks.
func (r ReverseReport) MedianMs(group string) int {
	var ms []int
	for _, c := range r.Checks {
		if c.Group == group && c.Proto == ProtoTCP && c.OK {
			ms = append(ms, c.Ms)
		}
	}
	if len(ms) == 0 {
		return 0
	}
	sort.Ints(ms)
	return ms[len(ms)/2]
}

// ReverseProber looks at whether this server can still get answers from Russia.
type ReverseProber interface {
	Probe(ctx context.Context) (ReverseReport, error)
}

// What the real prober asks by default. The Russian sites are spread over
// different companies and networks, and were picked because they accept a
// connection from abroad; the DNS servers answer anyone. Some of the sites do
// not answer a ping, which is why a ping is recorded but not judged.
var (
	defaultRUHosts = []string{
		"ya.ru", "vk.com", "mail.ru", "ozon.ru", "wildberries.ru", "avito.ru",
		"gosuslugi.ru", "sberbank.ru", "rbc.ru", "2gis.ru", "hh.ru", "kinopoisk.ru",
	}
	defaultControlHosts     = []string{"cloudflare.com", "github.com", "wikipedia.org", "debian.org"}
	defaultRUResolvers      = []string{"77.88.8.8", "77.88.8.1"}
	defaultControlResolvers = []string{"1.1.1.1", "8.8.8.8"}
)

// NetProber is the real thing, asking from this machine's own address.
type NetProber struct {
	RUHosts, ControlHosts         []string
	RUResolvers, ControlResolvers []string

	// Timeout is how long each single check may take.
	Timeout time.Duration

	// The four ways of asking, replaceable so that the rest can be tested without
	// a network.
	Lookup func(ctx context.Context, host string) (string, error)
	Dial   func(ctx context.Context, addr string) (time.Duration, error)
	Ping   func(ctx context.Context, ip string) (time.Duration, error)
	Query  func(ctx context.Context, server string) (time.Duration, error)
}

// NewNetProber returns a prober with the default targets.
func NewNetProber() *NetProber {
	return &NetProber{
		RUHosts:          defaultRUHosts,
		ControlHosts:     defaultControlHosts,
		RUResolvers:      defaultRUResolvers,
		ControlResolvers: defaultControlResolvers,
		Timeout:          3 * time.Second,
	}
}

// Probe implements ReverseProber. All the checks run at once, so a look takes
// about one timeout however many of them fail.
func (p *NetProber) Probe(ctx context.Context) (ReverseReport, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	lookup, dial, ping, query := p.Lookup, p.Dial, p.Ping, p.Query
	if lookup == nil {
		lookup = lookupIPv4
	}
	if dial == nil {
		dial = dialTCP443
	}
	if ping == nil {
		ping = pingOnce
	}
	if query == nil {
		query = queryDNS
	}

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		checks []Check
	)
	add := func(c Check) {
		mu.Lock()
		checks = append(checks, c)
		mu.Unlock()
	}
	run := func(group, proto, target string, ask func(context.Context) (time.Duration, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			d, err := ask(cctx)
			c := Check{Group: group, Proto: proto, Target: target, OK: err == nil, Ms: int(d / time.Millisecond)}
			if err != nil {
				c.Err = briefError(err)
			}
			add(c)
		}()
	}

	host := func(group, name string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lctx, cancel := context.WithTimeout(ctx, timeout)
			ip, err := lookup(lctx, name)
			cancel()
			if err != nil {
				// Without an address neither check can be made; both are
				// recorded as failed, with the reason, so that a name that does
				// not resolve is not mistaken for a path that is cut.
				add(Check{Group: group, Proto: ProtoTCP, Target: name, Err: "dns"})
				add(Check{Group: group, Proto: ProtoICMP, Target: name, Err: "dns"})
				return
			}
			run(group, ProtoTCP, name, func(c context.Context) (time.Duration, error) { return dial(c, ip) })
			run(group, ProtoICMP, name, func(c context.Context) (time.Duration, error) { return ping(c, ip) })
		}()
	}
	for _, h := range p.RUHosts {
		host(GroupRU, h)
	}
	for _, h := range p.ControlHosts {
		host(GroupControl, h)
	}
	for _, s := range p.RUResolvers {
		run(GroupRU, ProtoUDP, s, func(c context.Context) (time.Duration, error) { return query(c, s) })
	}
	for _, s := range p.ControlResolvers {
		run(GroupControl, ProtoUDP, s, func(c context.Context) (time.Duration, error) { return query(c, s) })
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return ReverseReport{}, err
	}
	// A stable order, so that two looks can be compared line by line.
	slices.SortFunc(checks, func(a, b Check) int {
		switch {
		case a.Group != b.Group:
			return compareStrings(a.Group, b.Group)
		case a.Target != b.Target:
			return compareStrings(a.Target, b.Target)
		}
		return compareStrings(a.Proto, b.Proto)
	})
	return ReverseReport{Checks: checks}, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// briefError turns an error into a word a log can keep.
func briefError(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	}
	s := err.Error()
	lower := strings.ToLower(s)
	for _, w := range []string{"refused", "unreachable", "reset"} {
		if strings.Contains(lower, w) {
			return w
		}
	}
	if strings.Contains(lower, "permit") {
		return "permission"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func lookupIPv4(ctx context.Context, host string) (string, error) {
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", errors.New("no address")
	}
	return ips[0].String(), nil
}

func dialTCP443(ctx context.Context, ip string) (time.Duration, error) {
	return dialTCP(ctx, net.JoinHostPort(ip, "443"))
}

func dialTCP(ctx context.Context, addr string) (time.Duration, error) {
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", addr)
	if err != nil {
		return 0, err
	}
	_ = conn.Close()
	return time.Since(start), nil
}

// queryDNS sends one real DNS question over UDP and waits for the answer to it.
func queryDNS(ctx context.Context, server string) (time.Duration, error) {
	return queryDNSAt(ctx, net.JoinHostPort(server, "53"))
}

func queryDNSAt(ctx context.Context, addr string) (time.Duration, error) {
	var idb [2]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return 0, err
	}
	id := binary.BigEndian.Uint16(idb[:])

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	if err := b.StartQuestions(); err != nil {
		return 0, err
	}
	if err := b.Question(dnsmessage.Question{
		Name: dnsmessage.MustNewName("ya.ru."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
	}); err != nil {
		return 0, err
	}
	packet, err := b.Finish()
	if err != nil {
		return 0, err
	}

	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp4", addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := conn.Write(packet); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return 0, err
		}
		var p dnsmessage.Parser
		h, err := p.Start(buf[:n])
		if err != nil || h.ID != id || !h.Response {
			continue // something else; keep waiting for the answer to ours
		}
		return time.Since(start), nil
	}
}

// pingOnce sends one echo request and waits for the reply. It needs the right to
// open a raw socket, which the manager has because it runs as root.
func pingOnce(ctx context.Context, ip string) (time.Duration, error) {
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	var idb [2]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return 0, err
	}
	id := int(binary.BigEndian.Uint16(idb[:]))
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: id, Seq: 1, Data: []byte("awgmgr")},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return 0, err
	}
	dst, err := net.ResolveIPAddr("ip4", ip)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	if _, err := conn.WriteTo(wire, dst); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		if peer.String() != ip {
			continue
		}
		reply, err := icmp.ParseMessage(1, buf[:n])
		if err != nil || reply.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		if echo, ok := reply.Body.(*icmp.Echo); ok && echo.ID == id {
			return time.Since(start), nil
		}
	}
}

// describeChecks is for tests and tools: one line per check.
func describeChecks(r ReverseReport) string {
	out := ""
	for _, c := range r.Checks {
		state := "ok"
		if !c.OK {
			state = "FAIL " + c.Err
		}
		out += fmt.Sprintf("%s %s %s %s %dms\n", c.Group, c.Target, c.Proto, state, c.Ms)
	}
	return out
}
