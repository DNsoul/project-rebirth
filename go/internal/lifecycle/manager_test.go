package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/fleet"
	"awgrotator/internal/proto"
)

func started(t *testing.T, r *rig) *Manager {
	t.Helper()
	m := r.manager()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return m
}

func tick(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func TestACommandMovesOnceAndIsNotRepeated(t *testing.T) {
	r := newRig(t)
	r.pub.inbox.Command = r.command(proto.ActionMove, "", r.clock.Now(), proto.IssuerRoot, nil)
	m := started(t, r)

	tick(t, m)
	if got := r.ev.count("provision"); got != 1 {
		t.Fatalf("%d servers built after the command, want 1", got)
	}
	if r.ev.count("publish") != 1 {
		t.Error("the new server was not published")
	}
	assertOnlyDeleted(t, r, r.cfg.InstanceID)

	// The command is still sitting in the gist. It must not run again.
	r.clock.Advance(2 * time.Minute)
	tick(t, m)
	tick(t, m)
	if got := r.ev.count("provision"); got != 1 {
		t.Errorf("the same command built %d servers", got)
	}
}

func TestAMoveSurvivesAManagerRestart(t *testing.T) {
	r := newRig(t)
	r.pub.inbox.Command = r.command(proto.ActionMove, "", r.clock.Now(), proto.IssuerRoot, nil)
	m := started(t, r)
	tick(t, m)

	// A fresh manager on the same journal, as after a restart.
	r.clock.Advance(time.Minute)
	m2 := started(t, r)
	tick(t, m2)
	if got := r.ev.count("provision"); got != 1 {
		t.Errorf("a restarted manager ran the command again: %d servers built", got)
	}
}

func TestACommandFromAnUntrustedKeyIsIgnored(t *testing.T) {
	r := newRig(t)
	stranger, err := proto.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	r.pub.inbox.Command = r.command(proto.ActionMove, "", r.clock.Now(), proto.IssuerRoot, stranger)
	m := started(t, r)

	tick(t, m)
	if r.ev.count("provision") != 0 {
		t.Fatal("a command signed by an unknown key built a server")
	}
}

func TestACommandFromTheBackupKeyWorks(t *testing.T) {
	r := newRig(t)
	r.pub.inbox.Command = r.command(proto.ActionMove, "", r.clock.Now(), proto.IssuerBackup, nil)
	m := started(t, r)

	tick(t, m)
	if r.ev.count("provision") != 1 {
		t.Error("the backup key could not give a command")
	}
}

func TestAStaleCommandIsIgnored(t *testing.T) {
	r := newRig(t)
	r.pub.inbox.Command = r.command(proto.ActionMove, "", r.clock.Now().Add(-time.Hour), proto.IssuerRoot, nil)
	m := started(t, r)

	tick(t, m)
	if r.ev.count("provision") != 0 {
		t.Fatal("a command an hour old built a server: replaying a captured command would work")
	}
}

func TestAMoveRegionCommandGoesToThatRegion(t *testing.T) {
	r := newRig(t)
	r.pub.inbox.Command = r.command(proto.ActionMoveRegion, "fra", r.clock.Now(), proto.IssuerRoot, nil)
	m := started(t, r)

	tick(t, m)
	if len(r.prov.reqs) != 1 || r.prov.reqs[0].Region != "fra" {
		t.Errorf("requests = %+v, want one for fra", r.prov.reqs)
	}
}

func TestAStatusCommandPublishesAStatus(t *testing.T) {
	r := newRig(t)
	m := started(t, r)
	tick(t, m) // the first status goes out at start-up
	before := r.ev.count("status")

	r.clock.Advance(time.Minute)
	r.pub.inbox.Command = r.command(proto.ActionStatus, "", r.clock.Now(), proto.IssuerRoot, nil)
	tick(t, m)
	if r.ev.count("status") <= before {
		t.Error("a status command did not produce a status")
	}
	if r.ev.count("provision") != 0 {
		t.Error("a status command moved the server")
	}
}

func TestThePlaceholderInboxIsIgnoredQuietly(t *testing.T) {
	r := newRig(t)
	r.pub.inbox = Inbox{Command: r.placeholder(), Revocation: r.placeholder()}
	m := started(t, r)

	tick(t, m)
	if r.ev.count("provision") != 0 || len(r.cloud.deletedIDs()) != 0 {
		t.Errorf("the empty placeholder caused something: %v", r.ev.all())
	}
}

func TestAutomaticMovesAreOffByDefault(t *testing.T) {
	r := newRig(t)
	r.clock.Advance(365 * 24 * time.Hour) // a year old
	r.prober.target = Reach{0, 10}        // and apparently blocked
	m := started(t, r)

	for i := 0; i < 6; i++ {
		r.clock.Advance(11 * time.Minute)
		tick(t, m)
	}
	if r.ev.count("provision") != 0 {
		t.Fatal("a server moved by itself although automatic moves are off")
	}
}

func TestAnOldServerMovesWhenAgeMovesAreOn(t *testing.T) {
	r := newRig(t)
	r.cfg.Policy.AutoAge = true
	r.clock.Advance(40 * 24 * time.Hour) // past the limit and any jitter
	m := started(t, r)

	tick(t, m)
	if r.ev.count("provision") != 1 {
		t.Fatalf("an old server did not move: %v", r.ev.all())
	}
	if a := lastAttempt(r); a.Trigger != TriggerAge {
		t.Errorf("trigger = %s, want age", a.Trigger)
	}
}

func TestAYoungServerStaysWhenAgeMovesAreOn(t *testing.T) {
	r := newRig(t)
	r.cfg.Policy.AutoAge = true
	r.clock.Advance(5 * 24 * time.Hour)
	m := started(t, r)

	tick(t, m)
	if r.ev.count("provision") != 0 {
		t.Error("a five day old server moved")
	}
}

func TestBlockedNeedsSeveralBadRoundsInARow(t *testing.T) {
	r := newRig(t)
	r.cfg.Policy.AutoBlock = true
	r.prober.target = Reach{0, 10}
	m := started(t, r)

	// BlockRounds is 3: two bad rounds are not enough.
	for i := 0; i < 2; i++ {
		r.clock.Advance(11 * time.Minute)
		tick(t, m)
	}
	if r.ev.count("provision") != 0 {
		t.Fatal("moved after only two bad rounds")
	}
	r.clock.Advance(11 * time.Minute)
	tick(t, m)
	if r.ev.count("provision") != 1 {
		t.Fatalf("did not move after three bad rounds: %v", r.ev.all())
	}
	if a := lastAttempt(r); a.Trigger != TriggerBlocked {
		t.Errorf("trigger = %s", a.Trigger)
	}
}

// If the control group cannot reach the server either, the measurement is
// broken, not the server.
func TestAFailingControlGroupNeverCountsAsBlocked(t *testing.T) {
	r := newRig(t)
	r.cfg.Policy.AutoBlock = true
	r.prober.target = Reach{0, 10}
	r.prober.control = Reach{1, 6}
	m := started(t, r)

	for i := 0; i < 10; i++ {
		r.clock.Advance(11 * time.Minute)
		tick(t, m)
	}
	if r.ev.count("provision") != 0 {
		t.Fatal("moved on evidence that the control group contradicted")
	}
}

func TestOneGoodRoundClearsTheBlockedCount(t *testing.T) {
	r := newRig(t)
	r.cfg.Policy.AutoBlock = true
	m := started(t, r)

	r.prober.target = Reach{0, 10}
	for i := 0; i < 2; i++ {
		r.clock.Advance(11 * time.Minute)
		tick(t, m)
	}
	r.prober.target = Reach{10, 10}
	r.clock.Advance(11 * time.Minute)
	tick(t, m)
	r.prober.target = Reach{0, 10}
	for i := 0; i < 2; i++ {
		r.clock.Advance(11 * time.Minute)
		tick(t, m)
	}
	if r.ev.count("provision") != 0 {
		t.Fatal("bad rounds were counted across a good one")
	}
}

func TestAServiceThatKeepsDyingIsRestartedAndThenMoved(t *testing.T) {
	r := newRig(t)
	r.cfg.Policy.AutoBlock = true
	r.host.running = false
	m := started(t, r)

	for i := 0; i < 3; i++ {
		r.clock.Advance(61 * time.Second)
		tick(t, m)
	}
	if r.host.restarts != 3 {
		t.Errorf("restarted %d times, want 3", r.host.restarts)
	}
	if r.ev.count("provision") != 1 {
		t.Fatalf("a server that would not stay up did not move: %v", r.ev.all())
	}
	if a := lastAttempt(r); a.Trigger != TriggerBroken {
		t.Errorf("trigger = %s", a.Trigger)
	}
}

func TestAServiceThatRecoversAfterARestartIsNotMoved(t *testing.T) {
	r := newRig(t)
	r.cfg.Policy.AutoBlock = true
	r.host.running = false
	m := started(t, r)

	r.clock.Advance(61 * time.Second)
	tick(t, m)
	r.host.running = true // the restart worked
	for i := 0; i < 5; i++ {
		r.clock.Advance(61 * time.Second)
		tick(t, m)
	}
	if r.ev.count("provision") != 0 {
		t.Error("moved although the service recovered")
	}
	if m.Verdict() != Healthy {
		t.Errorf("verdict = %s", m.Verdict())
	}
}

func TestAFailedMoveLeavesTheServerAloneAndIsReported(t *testing.T) {
	r := newRig(t)
	r.tester.report = failingReport()
	r.pub.inbox.Command = r.command(proto.ActionMove, "", r.clock.Now(), proto.IssuerRoot, nil)
	m := started(t, r)

	tick(t, m) // must not return an error: that would restart the process into trying again
	if !r.cloud.has(r.cfg.InstanceID) {
		t.Error("this server was removed after a failed move")
	}
	if r.ev.count("provision") != 1 {
		t.Errorf("%d attempts, want 1", r.ev.count("provision"))
	}

	// The failure shows up in the next status.
	r.clock.Advance(time.Minute)
	tick(t, m)
	var last StatusBody
	if n := len(r.pub.statuses); n > 0 {
		last = r.pub.statuses[n-1]
	}
	if last.LastMove == nil || last.LastMove.State != StateFailed || last.LastMove.Error == "" {
		t.Errorf("the status does not report the failed move: %+v", last.LastMove)
	}
}

// The limits that stop a recurring fault from turning into a loop of paid
// servers.
func TestARecurringFaultCannotRunAwayWithMoney(t *testing.T) {
	r := newRig(t)
	r.cfg.Policy.AutoBlock = true
	r.prober.target = Reach{0, 10} // permanently "blocked"
	r.tester.report = failingReport()
	m := started(t, r)

	for i := 0; i < 200; i++ { // about 33 hours at 10 minute steps
		r.clock.Advance(10 * time.Minute)
		tick(t, m)
	}

	j := r.j.Read()
	byDay := map[int64]int{}
	var last time.Time
	for _, a := range j.Attempts {
		at := time.Unix(a.StartedAt, 0)
		byDay[at.Unix()/86400]++
		if !last.IsZero() && at.Sub(last) < r.cfg.Policy.MinMoveInterval() {
			t.Errorf("two attempts %s apart, the minimum is %s", at.Sub(last), r.cfg.Policy.MinMoveInterval())
		}
		last = at
	}
	// The bound is on any rolling day, so check that directly.
	for i, a := range j.Attempts {
		start := time.Unix(a.StartedAt, 0)
		n := 0
		for _, b := range j.Attempts[i:] {
			if time.Unix(b.StartedAt, 0).Sub(start) < 24*time.Hour {
				n++
			}
		}
		if n > r.cfg.Policy.MaxMovesPerDay {
			t.Errorf("%d attempts within a day of %s, the limit is %d", n, start, r.cfg.Policy.MaxMovesPerDay)
		}
	}
	if len(j.Attempts) == 0 {
		t.Fatal("no attempts were made at all, so this test proves nothing")
	}
	if len(r.cloud.deletedIDs()) == 0 {
		t.Error("the failed servers were not cleaned up")
	}
	for _, i := range r.cloud.instances {
		if strings.HasPrefix(i.ID, "new-") {
			t.Errorf("a failed server %s was left running", i.ID)
		}
	}
}

func TestARevokedServerRemovesItself(t *testing.T) {
	r := newRig(t)
	blob, err := proto.SealRevocation(&proto.Revocation{MinEpoch: 2, IssuedAt: r.clock.Now().Unix()},
		proto.IssuerBackup, r.keys.backup, r.keys.content)
	if err != nil {
		t.Fatal(err)
	}
	r.pub.inbox.Revocation = blob
	m := started(t, r)

	tick(t, m)
	assertOnlyDeleted(t, r, r.cfg.InstanceID)
	if r.ev.count("provision") != 0 {
		t.Error("a revoked server built a successor")
	}
}

func TestARevocationBelowOurEpochChangesNothing(t *testing.T) {
	r := newRig(t) // the certificate is at epoch 1
	blob, err := proto.SealRevocation(&proto.Revocation{MinEpoch: 1, IssuedAt: r.clock.Now().Unix()},
		proto.IssuerRoot, r.keys.root, r.keys.content)
	if err != nil {
		t.Fatal(err)
	}
	r.pub.inbox.Revocation = blob
	m := started(t, r)

	tick(t, m)
	if len(r.cloud.deletedIDs()) != 0 {
		t.Error("a revocation that does not cover this server removed it")
	}
}

func TestARevocationFromAnUntrustedKeyIsIgnored(t *testing.T) {
	r := newRig(t)
	stranger, err := proto.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := proto.SealRevocation(&proto.Revocation{MinEpoch: 99, IssuedAt: r.clock.Now().Unix()},
		proto.IssuerRoot, stranger, r.keys.content)
	if err != nil {
		t.Fatal(err)
	}
	r.pub.inbox.Revocation = blob
	m := started(t, r)

	tick(t, m)
	if len(r.cloud.deletedIDs()) != 0 {
		t.Fatal("anyone who can write to the gist could kill the server")
	}
}

func heartbeatBody(t *testing.T, ip string) []byte {
	t.Helper()
	gen, err := fleet.NewGeneration(fleet.GenOptions{Split: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := gen.Body("sto", ip, fleet.ClientNet{DNS: []string{"198.18.0.1"}, AllowedIPs: []string{"198.18.0.0/15"}}, t0).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTheConfigurationIsRepublishedWeeklyWhileWeAreTheOnePublished(t *testing.T) {
	r := newRig(t)
	r.host.body = heartbeatBody(t, r.cfg.IP)
	r.pub.current = &fleet.ConfigBody{Endpoint: r.cfg.IP + ":4000"}
	m := started(t, r)

	tick(t, m)
	if r.ev.count("publish") != 0 {
		t.Fatal("republished at start-up, long before it is due")
	}
	r.clock.Advance(8 * 24 * time.Hour)
	tick(t, m)
	if r.ev.count("publish") != 1 {
		t.Errorf("%d republications after a week, want 1", r.ev.count("publish"))
	}
}

// A server waiting out its grace period must not overwrite the configuration
// of the server that replaced it.
func TestAReplacedServerNeverRepublishes(t *testing.T) {
	r := newRig(t)
	r.host.body = heartbeatBody(t, r.cfg.IP)
	r.pub.current = &fleet.ConfigBody{Endpoint: "198.51.100.9:4000"} // someone else is published
	m := started(t, r)

	r.clock.Advance(8 * 24 * time.Hour)
	tick(t, m)
	if r.ev.count("publish") != 0 {
		t.Fatal("a server that is no longer the published one overwrote the configuration")
	}
}

func TestNoRepublicationWhenTheGistCannotBeRead(t *testing.T) {
	r := newRig(t)
	r.host.body = heartbeatBody(t, r.cfg.IP)
	r.pub.currentErr = errors.New("unreachable")
	m := started(t, r)

	r.clock.Advance(8 * 24 * time.Hour)
	tick(t, m)
	if r.ev.count("publish") != 0 {
		t.Error("republished without knowing what was published")
	}
}

// The status is public to anyone with the app. It must carry states and
// counts, never an address.
func TestTheStatusLeaksNoAddressesOrNames(t *testing.T) {
	r := newRig(t)
	m := started(t, r)
	tick(t, m)

	if len(r.pub.statuses) == 0 {
		t.Fatal("no status was published")
	}
	raw, err := json.Marshal(r.pub.statuses[len(r.pub.statuses)-1])
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{r.cfg.IP, "198.51.100", "198.18.", "self-instance", r.cfg.GistID, r.cfg.VultrKey, r.cfg.GitHubToken} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the status contains %q:\n%s", forbidden, text)
		}
	}
}

func TestTheStatusSaysWhyTheServerIsStaying(t *testing.T) {
	r := newRig(t)
	m := started(t, r)
	tick(t, m)

	s := r.pub.statuses[len(r.pub.statuses)-1]
	if !strings.Contains(s.Why, "off") {
		t.Errorf("Why = %q, want it to say automatic moves are off", s.Why)
	}
	if s.AutoAge || s.AutoBlock || s.NextAutoMove != 0 {
		t.Errorf("the status claims automatic moves: %+v", s)
	}
	if s.Generation != 1 || s.Region != r.cfg.Region || s.Servers != 1 || s.CertDaysLeft < 300 {
		t.Errorf("status = %+v", s)
	}
}

func TestStartDrawsTheJitterOnceAndKeepsIt(t *testing.T) {
	r := newRig(t)
	started(t, r)
	first := r.j.Read().AgeJitterSec
	if first <= 0 || first > int64(r.cfg.Policy.AgeJitterHours)*3600 {
		t.Fatalf("jitter = %ds, want within (0, %dh]", first, r.cfg.Policy.AgeJitterHours)
	}

	started(t, r) // another manager on the same journal
	if again := r.j.Read().AgeJitterSec; again != first {
		t.Errorf("the jitter changed from %d to %d across restarts", first, again)
	}
}

func TestStatusIsPublishedDailyAndNotMoreOften(t *testing.T) {
	r := newRig(t)
	m := started(t, r)

	tick(t, m)
	first := r.ev.count("status")
	if first != 1 {
		t.Fatalf("%d statuses at start-up, want 1", first)
	}
	for i := 0; i < 20; i++ {
		r.clock.Advance(time.Hour)
		tick(t, m)
	}
	if got := r.ev.count("status"); got != 1 {
		t.Errorf("%d statuses within the first day, want only the one at start-up", got)
	}

	for i := 0; i < 10; i++ { // now past a day
		r.clock.Advance(time.Hour)
		tick(t, m)
	}
	if got := r.ev.count("status"); got != 2 {
		t.Errorf("%d statuses after a day and a half, want the first and one more", got)
	}
}

// A node that has only just started has not written its first status yet. The
// manager must not read that as a failure and restart the tunnel.
func TestANodeThatIsStillStartingIsNotRestarted(t *testing.T) {
	r := newRig(t)
	r.host.statusErr = errors.New("the node has not written a status yet")
	m := started(t, r)

	// Two checks, a minute apart each: the status has been missing for about
	// a minute and a bit, which is still inside the grace period.
	for i := 0; i < 2; i++ {
		r.clock.Advance(61 * time.Second)
		tick(t, m)
	}
	if r.host.restarts != 0 {
		t.Fatalf("restarted a node %d time(s) before the grace period was over", r.host.restarts)
	}
}

func TestANodeThatNeverStartsReportingIsRestartedAfterTheGrace(t *testing.T) {
	r := newRig(t)
	r.host.statusErr = errors.New("stale")
	m := started(t, r)

	for i := 0; i < 4; i++ {
		r.clock.Advance(61 * time.Second)
		tick(t, m)
	}
	if r.host.restarts == 0 {
		t.Fatal("a node that stopped reporting for minutes was never restarted")
	}
}

func TestANodeThatStartsReportingInTimeIsLeftAlone(t *testing.T) {
	r := newRig(t)
	r.host.statusErr = errors.New("not yet")
	m := started(t, r)

	r.clock.Advance(61 * time.Second)
	tick(t, m)
	r.host.statusErr = nil // it came up
	for i := 0; i < 5; i++ {
		r.clock.Advance(61 * time.Second)
		tick(t, m)
	}
	if r.host.restarts != 0 {
		t.Errorf("restarted a node that recovered on its own (%d)", r.host.restarts)
	}
	if m.Verdict() != Healthy {
		t.Errorf("verdict = %s", m.Verdict())
	}
}

func TestTheFirstLocalCheckWaitsAfterStart(t *testing.T) {
	r := newRig(t)
	r.host.running = false // would be restarted straight away if looked at
	m := started(t, r)

	tick(t, m) // immediately after start
	if r.host.restarts != 0 {
		t.Error("the node was judged in the first moments after the manager started")
	}
}

// Found by running it for real: the successor read the very command that had
// caused its own creation and tried to move again. It has to know, from its
// first moment, that the command has been dealt with.
func TestASuccessorDoesNotRepeatTheCommandThatCreatedIt(t *testing.T) {
	r := newRig(t)
	r.pub.inbox.Command = r.command(proto.ActionMove, "", r.clock.Now(), proto.IssuerRoot, nil)
	m := started(t, r)
	tick(t, m)
	if r.ev.count("provision") != 1 {
		t.Fatalf("setup: %d servers built", r.ev.count("provision"))
	}

	// What the first server handed to its successor.
	var seed []byte
	for _, f := range r.prov.files {
		if strings.HasSuffix(f.Path, "journal.json") {
			seed = f.Content
		}
	}
	if seed == nil {
		t.Fatalf("the successor was given no journal; files: %v", paths(r.prov.files))
	}

	// A second server in the same situation: same gist, with the command still
	// in it, and the journal the first one passed on.
	r2 := newRig(t)
	if err := writeText(r2.j.path, string(seed)); err != nil {
		t.Fatal(err)
	}
	j2, err := OpenJournal(r2.j.path)
	if err != nil {
		t.Fatal(err)
	}
	r2.j, r2.eng.J = j2, j2
	r2.keys, r2.cfg = r.keys, r.cfg
	r2.eng.Cfg = r.cfg
	r2.pub.inbox = r.pub.inbox
	m2 := started(t, r2)

	r2.clock.Advance(10 * time.Second)
	tick(t, m2)
	if got := r2.ev.count("provision"); got != 0 {
		t.Fatalf("the successor carried out its predecessor's command again: %d servers built", got)
	}
	// Not even an attempt: the command was recognised as already dealt with,
	// not tried and refused.
	if j := r2.j.Read(); len(j.Attempts) != 0 {
		t.Errorf("the successor recorded %d move attempt(s) for a command it should have ignored", len(j.Attempts))
	}
}

func TestTheSuccessorInheritsWhatItNeedsAndNothingElse(t *testing.T) {
	r := newRig(t)
	if err := r.j.Update(func(j *Journal) {
		j.MarkExecuted("cmd-1")
		j.MarkExecuted("cmd-2")
		j.MinEpoch = 3
		j.LastSeq = 41
		j.AgeJitterSec = 12345
		j.Attempts = []Attempt{{ID: "old", State: StateFailed}}
	}); err != nil {
		t.Fatal(err)
	}

	got := r.eng.inheritedJournal()
	if len(got.Executed) != 2 || !got.WasExecuted("cmd-1") || !got.WasExecuted("cmd-2") {
		t.Errorf("executed = %v", got.Executed)
	}
	if got.MinEpoch != 3 || got.LastSeq != 41 {
		t.Errorf("min epoch %d, last seq %d", got.MinEpoch, got.LastSeq)
	}
	// The successor draws its own age limit and starts with a clean history.
	if got.AgeJitterSec != 0 {
		t.Error("the age jitter was inherited; every generation would move on the same schedule")
	}
	if len(got.Attempts) != 0 {
		t.Error("the predecessor's attempts were inherited")
	}
}

// The manager keeps trying on its own: this is what removes a half-built
// server once the provider stops holding it locked.
func TestTheManagerKeepsTryingToRemoveAServerTheProviderHasLocked(t *testing.T) {
	r := newRig(t)
	r.cloud.add(vultrInstance("new-7"))
	r.pub.current = &fleet.ConfigBody{Endpoint: r.cfg.IP + ":1234"}
	if err := r.j.Begin(Attempt{ID: "a1", StartedAt: 1, State: StateStarted, InstanceID: "new-7"}); err != nil {
		t.Fatal(err)
	}
	r.cloud.deleteErr = errors.New("409 Conflict: Server is currently locked")
	m := started(t, r) // the first attempt at start-up fails, and must not stop it starting

	for i := 0; i < 3; i++ {
		r.clock.Advance(30 * time.Second)
		tick(t, m)
	}
	if !r.cloud.has("new-7") {
		t.Fatal("setup: the server should still be there while it is locked")
	}

	r.cloud.deleteErr = nil // the install finished, the lock is gone
	r.clock.Advance(30 * time.Second)
	tick(t, m)
	assertOnlyDeleted(t, r, "new-7")
	if a := lastAttempt(r); a.State != StateFailed {
		t.Errorf("state = %s", a.State)
	}
}

func TestNothingNewIsStartedWhileAnOldMoveIsUnsettled(t *testing.T) {
	r := newRig(t)
	r.cloud.add(vultrInstance("new-7"))
	r.pub.current = &fleet.ConfigBody{Endpoint: r.cfg.IP + ":1234"}
	if err := r.j.Begin(Attempt{ID: "a1", StartedAt: 1, State: StateProvisioned, InstanceID: "new-7"}); err != nil {
		t.Fatal(err)
	}
	r.cloud.deleteErr = errors.New("locked")
	r.pub.inbox.Command = r.command(proto.ActionMove, "", r.clock.Now(), proto.IssuerRoot, nil)
	m := started(t, r)

	r.clock.Advance(30 * time.Second)
	tick(t, m)
	if r.ev.count("provision") != 0 {
		t.Error("a new server was built while an earlier one was still waiting to be removed")
	}
}
