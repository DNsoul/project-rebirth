package lifecycle

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"awgrotator/internal/node"
)

// ---- reports

func report(ruOK, controlOK bool) ReverseReport {
	var r ReverseReport
	add := func(group, proto, target string, ok bool, ms int) {
		c := Check{Group: group, Proto: proto, Target: target, OK: ok, Ms: ms}
		if !ok {
			c.Err = "timeout"
			c.Ms = 0
		}
		r.Checks = append(r.Checks, c)
	}
	for i, h := range []string{"ya.ru", "vk.com", "mail.ru", "ozon.ru"} {
		add(GroupRU, ProtoTCP, h, ruOK, 20+i*10)
		// Some healthy sites never answer a ping.
		add(GroupRU, ProtoICMP, h, ruOK && i != 3, 25)
	}
	add(GroupRU, ProtoUDP, "77.88.8.8", ruOK, 60)
	add(GroupRU, ProtoUDP, "77.88.8.1", ruOK, 90)
	for _, h := range []string{"cloudflare.com", "github.com"} {
		add(GroupControl, ProtoTCP, h, controlOK, 5)
		add(GroupControl, ProtoICMP, h, controlOK, 5)
	}
	add(GroupControl, ProtoUDP, "1.1.1.1", controlOK, 1)
	return r
}

func TestAReportIsJudgedLikeTheOutsideCheck(t *testing.T) {
	cases := []struct {
		name         string
		ru, ctl      bool
		want         Round
		failed       int
		tcp, udp     Reach
		controlTotal int
	}{
		{"everything answers", true, true, Good, 0, Reach{4, 4}, Reach{2, 2}, 3},
		{"Russia is silent, the control is not", false, true, Bad, 6, Reach{0, 4}, Reach{0, 2}, 3},
		{"nothing answers anywhere: the measurement is at fault", false, false, Inconclusive, 6, Reach{0, 4}, Reach{0, 2}, 3},
	}
	for _, c := range cases {
		r := report(c.ru, c.ctl)
		if got := r.Round(); got != c.want {
			t.Errorf("%s: round %v, want %v", c.name, got, c.want)
		}
		if got := len(r.Failed()); got != c.failed {
			t.Errorf("%s: %d failed, want %d: %v", c.name, got, c.failed, r.Failed())
		}
		if got := r.Count(GroupRU, ProtoTCP); got != c.tcp {
			t.Errorf("%s: tcp %v, want %v", c.name, got, c.tcp)
		}
		if got := r.Count(GroupRU, ProtoUDP); got != c.udp {
			t.Errorf("%s: udp %v, want %v", c.name, got, c.udp)
		}
		if got := r.Count(GroupControl, ""); got.Total != c.controlTotal+2 {
			t.Errorf("%s: control total %d", c.name, got.Total)
		}
	}
}

func TestPingsDoNotDecideTheRound(t *testing.T) {
	// A healthy path where every site ignores a ping is still a good round, and
	// the failed list does not name sites for something they always do.
	r := report(true, true)
	for i := range r.Checks {
		if r.Checks[i].Proto == ProtoICMP {
			r.Checks[i].OK, r.Checks[i].Err = false, "timeout"
		}
	}
	if r.Round() != Good || len(r.Failed()) != 0 {
		t.Errorf("round %v, failed %v", r.Round(), r.Failed())
	}
	if got := r.Count(GroupRU, ProtoICMP); got.Reached != 0 || got.Total != 4 {
		t.Errorf("the pings are still counted for the record: %v", got)
	}
}

func TestTheMedianTimeShowsASlowPathBeforeABrokenOne(t *testing.T) {
	r := report(true, true)
	if got := r.MedianMs(GroupRU); got != 40 { // 20 30 40 50 -> index 2
		t.Errorf("median %d", got)
	}
	if got := report(false, true).MedianMs(GroupRU); got != 0 {
		t.Errorf("median with nothing answering: %d", got)
	}
}

// ---- the prober, with the network replaced

func TestTheProberAsksEveryTargetOverEveryProtocol(t *testing.T) {
	var dials, pings, queries atomic.Int32
	p := &NetProber{
		RUHosts:          []string{"a.ru", "b.ru"},
		ControlHosts:     []string{"c.org"},
		RUResolvers:      []string{"10.0.0.1"},
		ControlResolvers: []string{"10.0.0.2"},
		Timeout:          time.Second,
		Lookup: func(_ context.Context, host string) (string, error) {
			if host == "b.ru" {
				return "", errors.New("no such host")
			}
			return "192.0.2.1", nil
		},
		Dial: func(context.Context, string) (time.Duration, error) {
			dials.Add(1)
			return 12 * time.Millisecond, nil
		},
		Ping: func(context.Context, string) (time.Duration, error) {
			pings.Add(1)
			return 0, errors.New("operation not permitted: permission denied")
		},
		Query: func(_ context.Context, s string) (time.Duration, error) {
			queries.Add(1)
			if s == "10.0.0.1" {
				return 0, context.DeadlineExceeded
			}
			return 30 * time.Millisecond, nil
		},
	}
	r, err := p.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 || pings.Load() != 2 || queries.Load() != 2 {
		t.Errorf("dials %d, pings %d, queries %d: a name that does not resolve gets neither", dials.Load(), pings.Load(), queries.Load())
	}
	byKey := map[string]Check{}
	for _, c := range r.Checks {
		byKey[c.Group+"/"+c.Target+"/"+c.Proto] = c
	}
	if c := byKey["ru/a.ru/tcp"]; !c.OK || c.Ms != 12 {
		t.Errorf("a.ru tcp: %+v", c)
	}
	if c := byKey["ru/b.ru/tcp"]; c.OK || c.Err != "dns" {
		t.Errorf("a name that does not resolve is recorded as such: %+v", c)
	}
	if c := byKey["ru/a.ru/icmp"]; c.OK || c.Err != "permission" {
		t.Errorf("icmp without the right: %+v", c)
	}
	if c := byKey["ru/10.0.0.1/udp"]; c.OK || c.Err != "timeout" {
		t.Errorf("udp that times out: %+v", c)
	}
	if c := byKey["control/10.0.0.2/udp"]; !c.OK || c.Ms != 30 {
		t.Errorf("control udp: %+v", c)
	}

	// The same look twice lists the same things in the same order.
	again, _ := p.Probe(context.Background())
	if describeChecks(again) != describeChecks(r) {
		t.Errorf("the order is not stable:\n%s\nvs\n%s", describeChecks(r), describeChecks(again))
	}
}

func TestEveryCheckHasItsOwnTimeoutAndTheyRunAtOnce(t *testing.T) {
	hang := func(ctx context.Context, _ string) (time.Duration, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	p := &NetProber{
		RUHosts:      []string{"a.ru", "b.ru", "c.ru", "d.ru", "e.ru", "f.ru"},
		ControlHosts: []string{"x.org", "y.org"},
		Timeout:      80 * time.Millisecond,
		Lookup:       func(context.Context, string) (string, error) { return "192.0.2.1", nil },
		Dial:         hang, Ping: hang, Query: hang,
	}
	started := time.Now()
	r, err := p.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("a look at 16 silent targets took %s: they are not being asked together", took)
	}
	for _, c := range r.Checks {
		if c.OK || c.Err != "timeout" {
			t.Errorf("%+v", c)
		}
	}
	if len(r.Checks) != 16 {
		t.Errorf("%d checks", len(r.Checks))
	}
}

func TestACancelledLookIsNotAReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &NetProber{
		RUHosts: []string{"a.ru"},
		Lookup:  func(context.Context, string) (string, error) { return "192.0.2.1", nil },
		Dial:    func(ctx context.Context, _ string) (time.Duration, error) { return 0, ctx.Err() },
		Ping:    func(ctx context.Context, _ string) (time.Duration, error) { return 0, ctx.Err() },
		Query:   func(ctx context.Context, _ string) (time.Duration, error) { return 0, ctx.Err() },
	}
	if _, err := p.Probe(ctx); err == nil {
		t.Error("a look that was cut short was taken for a finding")
	}
}

// ---- the real ways of asking, against a local network

func TestDialingTellsAnOpenPortFromAClosedOne(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dialTCP(ctx, l.Addr().String()); err != nil {
		t.Fatalf("an open port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	_, err = dialTCP(ctx, addr)
	if err == nil {
		t.Fatal("a closed port answered")
	}
	if got := briefError(err); got != "refused" {
		t.Errorf("a closed port is called %q", got)
	}
}

// dnsServer answers every question it gets, after an optional wrong answer.
func dnsServer(t *testing.T, wrongFirst bool) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			q := append([]byte(nil), buf[:n]...)
			q[2] |= 0x80 // QR: a response
			if wrongFirst {
				other := append([]byte(nil), q...)
				binary.BigEndian.PutUint16(other[:2], binary.BigEndian.Uint16(q[:2])^0xffff)
				_, _ = pc.WriteTo(other, from)
			}
			_, _ = pc.WriteTo(q, from)
		}
	}()
	return pc.LocalAddr().String()
}

func TestAnUdpQuestionIsAnsweredOnlyByItsOwnAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := queryDNSAt(ctx, dnsServer(t, false)); err != nil {
		t.Fatalf("a server that answers: %v", err)
	}
	if _, err := queryDNSAt(ctx, dnsServer(t, true)); err != nil {
		t.Fatalf("an answer for something else comes first and is skipped: %v", err)
	}

	// Nobody answers: the deadline is what ends it.
	silent, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	short, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	_, err = queryDNSAt(short, silent.LocalAddr().String())
	if err == nil || briefError(err) != "timeout" {
		t.Errorf("a silent server: %v", err)
	}
}

// ---- the log

func sampleAt(at int64, round string) Sample {
	return Sample{At: at, Gen: 6, Round: round, Health: "healthy",
		RU:  Tally{TCP: [2]int{12, 12}, UDP: [2]int{2, 2}, ICMP: [2]int{9, 12}},
		Ctl: Tally{TCP: [2]int{4, 4}, UDP: [2]int{2, 2}, ICMP: [2]int{4, 4}}}
}

func TestTheLogKeepsLinesAndReadsThemBack(t *testing.T) {
	l := &WatchLog{Path: filepath.Join(t.TempDir(), "sub", "watch.jsonl")}
	for i := 0; i < 5; i++ {
		s := sampleAt(int64(1000+i*120), "good")
		if i == 3 {
			s.Round, s.Failed = "bad", []string{"ya.ru/tcp", "77.88.8.8/udp"}
			s.Peers = &PeerTally{Known: 3, Up: 1, NewestS: 40, RxBytes: 10, TxBytes: 20}
		}
		if err := l.Append(s); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := l.Tail(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	got := ParseSamples(raw)
	if len(got) != 5 || got[3].Round != "bad" || got[3].Peers == nil || got[3].Peers.Up != 1 || len(got[3].Failed) != 2 {
		t.Fatalf("read back %+v", got)
	}
	if strings.Count(string(raw), "\n") != 5 {
		t.Errorf("one line each: %q", raw)
	}
	if info, err := os.Stat(l.Path); err != nil || info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("the log should be private: %v %v", info, err)
	}
}

func TestTheLogIsSetAsideWhenItGetsBigAndTwoFilesAreKept(t *testing.T) {
	l := &WatchLog{Path: filepath.Join(t.TempDir(), "watch.jsonl"), MaxBytes: 600}
	for i := 0; i < 20; i++ {
		if err := l.Append(sampleAt(int64(1000+i), "good")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(l.Path + ".1"); err != nil {
		t.Fatalf("nothing was set aside: %v", err)
	}
	for _, p := range []string{l.Path, l.Path + ".1"} {
		info, err := os.Stat(p)
		if err != nil || info.Size() > 600 {
			t.Errorf("%s: %v size %v", p, err, info)
		}
	}
	if _, err := os.Stat(l.Path + ".2"); err == nil {
		t.Error("more than two files are kept")
	}

	raw, _ := l.Tail(1 << 20)
	got := ParseSamples(raw)
	if len(got) < 5 || got[len(got)-1].At != 1019 {
		t.Fatalf("%d samples, the last at %d: the newest must be kept", len(got), got[len(got)-1].At)
	}
	for i := 1; i < len(got); i++ {
		if got[i].At != got[i-1].At+1 {
			t.Fatalf("the older file comes first and nothing is lost between them: %d then %d", got[i-1].At, got[i].At)
		}
	}
}

func TestTheEndOfTheLogStartsAtAWholeLine(t *testing.T) {
	l := &WatchLog{Path: filepath.Join(t.TempDir(), "watch.jsonl")}
	for i := 0; i < 30; i++ {
		_ = l.Append(sampleAt(int64(2000+i), "good"))
	}
	one, _ := os.ReadFile(l.Path)
	lineLen := len(one) / 30
	tail, err := l.Tail(lineLen*4 + lineLen/2)
	if err != nil {
		t.Fatal(err)
	}
	got := ParseSamples(tail)
	if len(got) != 4 || got[3].At != 2029 {
		t.Errorf("%d lines, last at %d, from %q…", len(got), got[len(got)-1].At, tail[:20])
	}
	if none, err := (&WatchLog{Path: filepath.Join(t.TempDir(), "absent")}).Tail(100); err != nil || none != nil {
		t.Errorf("no log: %v %v", none, err)
	}
}

func TestADamagedLineDoesNotHideTheRest(t *testing.T) {
	good, _ := json.Marshal(sampleAt(5000, "good"))
	raw := append([]byte("{\"t\":4999,\"round\":\"go\n"), good...)
	raw = append(raw, []byte("\nnot json at all\n\n")...)
	if got := ParseSamples(raw); len(got) != 1 || got[0].At != 5000 {
		t.Errorf("%+v", got)
	}
}

// ---- the manager keeps the record

type fakeReverse struct {
	report ReverseReport
	err    error
	looks  int
}

func (f *fakeReverse) Probe(context.Context) (ReverseReport, error) {
	f.looks++
	return f.report, f.err
}

// watched starts a manager with an availability log in a temporary directory.
func watched(t *testing.T, r *rig, rev *fakeReverse) (*Manager, *WatchLog) {
	t.Helper()
	m := started(t, r)
	log := &WatchLog{Path: filepath.Join(t.TempDir(), WatchLogName)}
	m.Watch, m.Reverse = log, rev
	return m, log
}

func samplesOf(t *testing.T, l *WatchLog) []Sample {
	t.Helper()
	raw, err := l.Tail(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	return ParseSamples(raw)
}

func TestTheManagerWritesALineEveryFewMinutes(t *testing.T) {
	r := newRig(t)
	r.host.status = node.Status{UpdatedAt: r.clock.Now(), Peers: []node.PeerStatus{
		{LastHandshake: r.clock.Now().Add(-30 * time.Second), RxBytes: 100, TxBytes: 300},
		{LastHandshake: r.clock.Now().Add(-time.Hour), RxBytes: 5, TxBytes: 7},
	}}
	rev := &fakeReverse{report: report(true, true)}
	m, log := watched(t, r, rev)

	tick(t, m)
	tick(t, m) // nothing is due yet
	if rev.looks != 1 {
		t.Fatalf("%d looks on the first pass, want 1", rev.looks)
	}
	r.clock.Advance(119 * time.Second)
	tick(t, m)
	if rev.looks != 1 {
		t.Fatalf("looked again after 119 seconds (%d)", rev.looks)
	}
	r.clock.Advance(2 * time.Second)
	r.host.status.UpdatedAt = r.clock.Now()
	tick(t, m)
	if rev.looks != 2 {
		t.Fatalf("%d looks after two minutes, want 2", rev.looks)
	}

	got := samplesOf(t, log)
	if len(got) != 2 {
		t.Fatalf("%d lines", len(got))
	}
	first := got[0]
	if first.Round != "good" || first.Gen != r.cfg.Generation || first.Health != "healthy" ||
		first.RU.TCP != [2]int{4, 4} || first.RU.UDP != [2]int{2, 2} || first.RU.ICMP != [2]int{3, 4} ||
		first.Ctl.TCP != [2]int{2, 2} || first.RUms != 40 || len(first.Failed) != 0 {
		t.Errorf("first line: %+v", first)
	}
	if p := first.Peers; p == nil || p.Known != 2 || p.Up != 1 || p.NewestS != 30 || p.RxBytes != 105 || p.TxBytes != 307 {
		t.Errorf("what the clients were doing: %+v", first.Peers)
	}
}

func TestAChangeIsRecordedAndPublishedSoonButNotAtOnceAndNotRepeatedly(t *testing.T) {
	r := newRig(t)
	rev := &fakeReverse{report: report(true, true)}
	m, log := watched(t, r, rev)

	tick(t, m) // first look and the first status
	statuses := len(r.pub.statuses)
	if statuses == 0 || r.pub.statuses[0].Watch == nil || r.pub.statuses[0].Watch.Round != "good" {
		t.Fatalf("the first status should carry the watch: %+v", r.pub.statuses)
	}
	if len(r.pub.statuses[0].Watch.Changes) != 0 {
		t.Errorf("the first look is a baseline, not a change: %+v", r.pub.statuses[0].Watch.Changes)
	}

	// Russia goes silent.
	rev.report = report(false, true)
	r.clock.Advance(2 * time.Minute)
	tick(t, m)
	if len(r.pub.statuses) != statuses {
		t.Fatalf("published at once, only two minutes after the last status")
	}
	r.clock.Advance(9 * time.Minute)
	tick(t, m) // more than ten minutes since the last status
	if len(r.pub.statuses) != statuses+1 {
		t.Fatalf("the change was not published after the gap: %d statuses", len(r.pub.statuses))
	}
	w := r.pub.statuses[len(r.pub.statuses)-1].Watch
	if w == nil || w.Round != "bad" || len(w.Changes) != 1 || w.Changes[0].To != "bad" || w.Changes[0].What != "reverse" {
		t.Fatalf("the published summary: %+v", w)
	}
	if !strings.Contains(w.RU, "tcp 0/4") {
		t.Errorf("the counts: %+v", w)
	}

	// Nothing new: no more statuses until the daily one.
	r.clock.Advance(20 * time.Minute)
	tick(t, m)
	if len(r.pub.statuses) != statuses+1 {
		t.Errorf("a status was published again with nothing new")
	}

	got := samplesOf(t, log)
	var bad *Sample
	for i := range got {
		if got[i].Round == "bad" {
			bad = &got[i]
			break
		}
	}
	if bad == nil || len(bad.Failed) != 6 || !slices.Contains(bad.Failed, "ya.ru/tcp") || !slices.Contains(bad.Failed, "77.88.8.8/udp") {
		t.Fatalf("the failing targets belong in the log: %+v", bad)
	}
}

func TestWhatTheWatchSeesNeverReachesThePublishedStatusAsAName(t *testing.T) {
	r := newRig(t)
	rev := &fakeReverse{report: report(false, true)}
	m, _ := watched(t, r, rev)
	tick(t, m)
	r.clock.Advance(15 * time.Minute)
	rev.report = report(true, true)
	tick(t, m)

	raw, err := json.Marshal(r.pub.statuses[len(r.pub.statuses)-1])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"ya.ru", "vk.com", "mail.ru", "77.88", "cloudflare", r.cfg.IP} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("the status names %q:\n%s", forbidden, raw)
		}
	}
}

func TestNotBeingAbleToLookIsNotAFinding(t *testing.T) {
	r := newRig(t)
	rev := &fakeReverse{report: report(true, true)}
	m, log := watched(t, r, rev)
	tick(t, m)

	rev.err = context.DeadlineExceeded
	r.clock.Advance(3 * time.Minute)
	tick(t, m)
	rev.err = nil
	r.clock.Advance(3 * time.Minute)
	tick(t, m)

	got := samplesOf(t, log)
	if len(got) != 3 || got[1].Round != "unknown" || got[1].Err != "timeout" {
		t.Fatalf("%+v", got)
	}
	if len(m.watch.changes) != 0 {
		t.Errorf("a look that could not run was recorded as a change: %+v", m.watch.changes)
	}
}

func TestTheOutsideCheckAppearsBesideTheServersOwnLook(t *testing.T) {
	r := newRig(t)
	rev := &fakeReverse{report: report(true, true)}
	m, log := watched(t, r, rev)

	tick(t, m) // the outside check runs on the first pass too
	if r.prober.calls != 1 {
		t.Fatalf("%d outside checks", r.prober.calls)
	}
	r.clock.Advance(3 * time.Minute)
	tick(t, m)
	r.prober.target = Reach{0, 10} // Russia stops reaching the server
	r.clock.Advance(11 * time.Minute)
	tick(t, m)
	r.clock.Advance(3 * time.Minute)
	tick(t, m)

	got := samplesOf(t, log)
	var withExt []Sample
	for _, s := range got {
		if s.Ext != nil {
			withExt = append(withExt, s)
		}
	}
	if len(withExt) != 2 {
		t.Fatalf("the outside check should be on the line after it ran, once each: %+v", withExt)
	}
	if withExt[0].Ext.Round != "good" || withExt[1].Ext.Round != "bad" || withExt[1].Ext.RU != [2]int{0, 10} {
		t.Errorf("%+v / %+v", withExt[0].Ext, withExt[1].Ext)
	}
	var external []WatchChange
	for _, c := range m.watch.changes {
		if c.What == "external" {
			external = append(external, c)
		}
	}
	if len(external) != 1 || external[0].To != "bad" {
		t.Errorf("the change in the outside check should be on record once: %+v", m.watch.changes)
	}
}

func TestTheRecordNeverMakesTheServerMove(t *testing.T) {
	// The whole point of the log is that it only watches. Even with moves on
	// block switched on and the reverse probe reporting a block for hours, the
	// decision rests on the outside check alone.
	r := newRig(t)
	r.cfg.Policy.AutoBlock = true
	r.host.status = node.Status{UpdatedAt: r.clock.Now()}
	rev := &fakeReverse{report: report(false, true)}
	m, _ := watched(t, r, rev)

	for i := 0; i < 40; i++ {
		r.clock.Advance(3 * time.Minute)
		r.host.status.UpdatedAt = r.clock.Now()
		tick(t, m)
	}
	if r.ev.count("provision") != 0 || len(r.cloud.deletedIDs()) != 0 {
		t.Fatalf("the server moved on the strength of the log: %v", r.ev.all())
	}
	if m.tracker.Verdict() != Healthy {
		t.Errorf("the tracker was fed by the watch: %s", m.tracker.Verdict())
	}
}

func TestALogThatCannotBeWrittenDoesNotStopTheManager(t *testing.T) {
	r := newRig(t)
	rev := &fakeReverse{report: report(true, true)}
	m := started(t, r)
	// A directory where the file should be.
	blocked := filepath.Join(t.TempDir(), WatchLogName)
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	m.Watch, m.Reverse = &WatchLog{Path: blocked}, rev

	for i := 0; i < 3; i++ {
		r.clock.Advance(3 * time.Minute)
		tick(t, m)
	}
	if rev.looks != 3 || len(r.pub.statuses) == 0 {
		t.Errorf("looks %d, statuses %d", rev.looks, len(r.pub.statuses))
	}
}

func TestWithoutALogNothingChanges(t *testing.T) {
	r := newRig(t)
	m := started(t, r)
	tick(t, m)
	if len(r.pub.statuses) == 0 || r.pub.statuses[0].Watch != nil {
		t.Errorf("a status without a log should say nothing about one: %+v", r.pub.statuses)
	}
}

// ---- a replacement carries on from the record

func TestTheSuccessorIsGivenTheEndOfTheLog(t *testing.T) {
	r := newRig(t)
	r.host.watchLog = []byte(`{"t":1,"round":"good"}` + "\n")
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	var got []byte
	for _, f := range r.prov.files {
		if strings.HasSuffix(f.Path, WatchLogName) {
			got = f.Content
		}
	}
	if string(got) != string(r.host.watchLog) {
		t.Fatalf("the successor was not given the log; files: %v", paths(r.prov.files))
	}
}

func TestAMoveWithNoLogGivesTheSuccessorNone(t *testing.T) {
	r := newRig(t)
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})
	for _, f := range r.prov.files {
		if strings.HasSuffix(f.Path, WatchLogName) {
			t.Errorf("an empty log was handed over: %s", f.Path)
		}
	}
}

func TestTheSystemHostReadsTheEndOfItsOwnLog(t *testing.T) {
	cfg := testConfig(t, newTestKeys(t))
	cfg.DataDir = t.TempDir()
	h := &SystemHost{Cfg: cfg}
	if raw, err := h.ReadWatchLog(); err != nil || raw != nil {
		t.Fatalf("no log yet: %q %v", raw, err)
	}
	l := &WatchLog{Path: filepath.Join(cfg.Data(), WatchLogName)}
	if err := l.Append(sampleAt(7000, "good")); err != nil {
		t.Fatal(err)
	}
	raw, err := h.ReadWatchLog()
	if err != nil || len(ParseSamples(raw)) != 1 {
		t.Errorf("%q %v", raw, err)
	}
}

func TestTheStatusSaysWhatTheWatchSees(t *testing.T) {
	s := StatusBody{Version: StatusVersion, Generation: 6, Region: "sto", Health: Healthy, Watch: &WatchSummary{
		Round: "bad", Since: 1_800_000_000, RU: "tcp 0/12, udp 0/2", Control: "tcp 4/4, udp 2/2",
		Changes: []WatchChange{{At: 1_800_000_000, To: "bad", What: "reverse"}, {At: 1_800_000_600, To: "bad", What: "external"}},
	}}
	out := s.Describe(time.Unix(1_800_001_000, 0))
	for _, want := range []string{"НЕ отвечают", "tcp 0/12", "проба сервера", "проверка с российских точек", "ни на что не влияет"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A status from a server that has no log reads as before.
	if plain := (StatusBody{Version: StatusVersion}).Describe(time.Now()); strings.Contains(plain, "российскими") {
		t.Errorf("%s", plain)
	}
	raw, _ := json.Marshal(StatusBody{Version: StatusVersion})
	if strings.Contains(string(raw), "watch") {
		t.Errorf("an absent watch should not be written: %s", raw)
	}
}
