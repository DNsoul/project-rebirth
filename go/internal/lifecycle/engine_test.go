package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/awg"
	"awgrotator/internal/enroll"
	"awgrotator/internal/fleet"
	"awgrotator/internal/node"
	"awgrotator/internal/proto"
)

func mustMove(t *testing.T, r *rig, req MoveRequest) *MoveResult {
	t.Helper()
	res, err := r.eng.Move(context.Background(), req)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	return res
}

func TestAMoveBuildsTestsAndPublishesInThatOrder(t *testing.T) {
	r := newRig(t)
	res := mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	want := []string{"tagged", "provision", "test", "publish"}
	got := r.ev.all()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}

	if res.InstanceID != "new-1" || res.IP != "198.51.100.1" {
		t.Errorf("result = %+v", res)
	}
	if res.Region == r.cfg.Region {
		t.Errorf("the new server is in the same region as this one: %s", res.Region)
	}

	// Moving does not retire this server: that is a separate, later step.
	if !r.cloud.has(r.cfg.InstanceID) || len(r.cloud.deletedIDs()) != 0 {
		t.Error("this server was deleted by Move; it must wait for Retire")
	}

	j := r.j.Read()
	a := j.Latest()
	if a == nil || a.State != StatePublished || a.InstanceID != "new-1" || a.RetireAt == 0 {
		t.Fatalf("journal = %+v", a)
	}
	if want := time.Unix(a.PublishedAt, 0).Add(r.cfg.Policy.Grace()).Unix(); a.RetireAt != want {
		t.Errorf("RetireAt = %d, want published + grace = %d", a.RetireAt, want)
	}
}

func TestThePublishedConfigPointsAtTheNewServer(t *testing.T) {
	r := newRig(t)
	res := mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	if len(r.pub.published) != 1 {
		t.Fatalf("%d configurations published", len(r.pub.published))
	}
	body := r.pub.published[0]
	if endpointHost(body.Endpoint) != res.IP {
		t.Errorf("the published endpoint is %s, the new server is %s", body.Endpoint, res.IP)
	}
	if err := body.Validate(); err != nil {
		t.Errorf("the published body is not valid: %v", err)
	}
	if body.Region != res.Region {
		t.Errorf("published region %s, new server %s", body.Region, res.Region)
	}
	// Split routing: the client is sent only the stand-in range.
	if len(body.DNS) != 1 || body.DNS[0] != "198.18.0.1" {
		t.Errorf("DNS = %v", body.DNS)
	}
}

// What the new server is tested with has to be what it was built with: its own
// key, its own port and its own obfuscation parameters.
func TestTheNewServerIsTestedWithItsOwnIdentity(t *testing.T) {
	r := newRig(t)
	res := mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	req := r.prov.reqs[0]
	target := r.tester.target
	installed, err := awg.PublicFrom(req.Node.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if target.ServerPublic != installed {
		t.Error("the test used a different server key than the one installed")
	}
	if int(target.Endpoint.Port()) != req.Node.ListenPort {
		t.Errorf("tested port %d, installed port %d", target.Endpoint.Port(), req.Node.ListenPort)
	}
	if target.Endpoint.Addr().String() != res.IP {
		t.Errorf("tested %s, the new server is %s", target.Endpoint.Addr(), res.IP)
	}
	if target.Params != req.Node.Params {
		t.Error("the test used different obfuscation parameters than the server runs with")
	}
}

func TestEveryGenerationGetsFreshKeys(t *testing.T) {
	r := newRig(t)
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	// Clear the room and the journal so a second move is allowed.
	r.cloud.instances = r.cloud.instances[:1]
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	a, b := r.prov.reqs[0].Node, r.prov.reqs[1].Node
	if a.PrivateKey == b.PrivateKey {
		t.Error("two generations share a server key")
	}
	if a.Params.HeaderProtectionKey == b.Params.HeaderProtectionKey {
		t.Error("two generations share a header protection key")
	}
}

func TestTheSuccessorBecomesAManagerInItsOwnRight(t *testing.T) {
	r := newRig(t)
	res := mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	byPath := map[string][]byte{}
	for _, f := range r.prov.files {
		byPath[f.Path] = f.Content
	}

	raw, ok := byPath[DefaultConfigPath]
	if !ok {
		t.Fatalf("the successor was not given a manager configuration; files: %v", paths(r.prov.files))
	}
	path := t.TempDir() + "/manager.json"
	if err := writeText(path, string(raw)); err != nil {
		t.Fatal(err)
	}
	next, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the successor's manager configuration is not valid: %v", err)
	}

	if next.InstanceID != res.InstanceID || next.IP != res.IP {
		t.Errorf("the successor believes it is %s/%s, it is %s/%s", next.InstanceID, next.IP, res.InstanceID, res.IP)
	}
	if next.Region != res.Region {
		t.Errorf("region %s, want %s", next.Region, res.Region)
	}
	if next.Generation != r.cfg.Generation+1 {
		t.Errorf("generation %d, want %d", next.Generation, r.cfg.Generation+1)
	}
	if next.CreatedAt != r.clock.Now().Unix() {
		t.Errorf("the successor's life began at %d, not now (%d)", next.CreatedAt, r.clock.Now().Unix())
	}
	// It carries on with the same credentials, key line and policy.
	if next.VultrKey != r.cfg.VultrKey || next.ServerSeed != r.cfg.ServerSeed || next.Cert != r.cfg.Cert {
		t.Error("the successor lost the credentials or the certificate")
	}
	if next.Policy != r.cfg.Policy {
		t.Errorf("policy changed: %+v vs %+v", next.Policy, r.cfg.Policy)
	}

	if _, ok := byPath[DataPath(next, "body.json")]; !ok {
		t.Errorf("the successor was not given the configuration to republish; files: %v", paths(r.prov.files))
	}
	if pool := byPath[node.DefaultPoolPath]; string(pool) != string(r.host.pool) {
		t.Error("the table of stand-in addresses was not passed on")
	}
}

func paths(files []deployFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func TestTheSuccessorSavedBodyIsWhatWasPublished(t *testing.T) {
	r := newRig(t)
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	var saved []byte
	for _, f := range r.prov.files {
		if strings.HasSuffix(f.Path, "body.json") {
			saved = f.Content
		}
	}
	if saved == nil {
		t.Fatal("no body.json for the successor")
	}
	body, err := fleet.ParseBody(saved)
	if err != nil {
		t.Fatalf("the saved body is not usable: %v", err)
	}
	if body.Endpoint != r.pub.published[0].Endpoint {
		t.Errorf("saved endpoint %s, published %s", body.Endpoint, r.pub.published[0].Endpoint)
	}
}

func TestAMoveIsRefusedAtTheServerCeiling(t *testing.T) {
	r := newRig(t)
	for i := 0; i < r.cfg.Policy.MaxServers; i++ {
		r.cloud.add(vultrInstance("extra"))
	}

	_, err := r.eng.Move(context.Background(), MoveRequest{Trigger: TriggerCommand})
	if !errors.Is(err, ErrCapReached) {
		t.Fatalf("err = %v, want ErrCapReached", err)
	}
	if r.ev.count("provision") != 0 {
		t.Error("a server was built although the ceiling was reached")
	}
	if a := lastAttempt(r); a.State != StateFailed || !strings.Contains(a.Error, "limit") {
		t.Errorf("journal = %+v", a)
	}
}

func TestAFailedBuildPublishesNothingAndDeletesNothing(t *testing.T) {
	r := newRig(t)
	r.prov.err = errors.New("vultr said no")

	_, err := r.eng.Move(context.Background(), MoveRequest{Trigger: TriggerCommand})
	if err == nil || !strings.Contains(err.Error(), "vultr said no") {
		t.Fatalf("err = %v", err)
	}
	if r.ev.count("test") != 0 || r.ev.count("publish") != 0 {
		t.Errorf("went on after a failed build: %v", r.ev.all())
	}
	// Provisioning removes its own leftovers; the engine must not delete.
	if len(r.cloud.deletedIDs()) != 0 {
		t.Errorf("deleted %v after a failed build", r.cloud.deletedIDs())
	}
	if a := lastAttempt(r); a.State != StateFailed || a.Error == "" {
		t.Errorf("journal = %+v", a)
	}
}

func TestAFailedTunnelTestRemovesTheNewServerAndPublishesNothing(t *testing.T) {
	r := newRig(t)
	r.tester.report = failingReport()

	_, err := r.eng.Move(context.Background(), MoveRequest{Trigger: TriggerCommand})
	if err == nil || !strings.Contains(err.Error(), "failed the tunnel test") {
		t.Fatalf("err = %v", err)
	}
	if r.ev.count("publish") != 0 {
		t.Error("a server that failed its test was published")
	}
	assertOnlyDeleted(t, r, "new-1")
	if !r.cloud.has(r.cfg.InstanceID) {
		t.Error("the current server is gone")
	}
}

func TestATestThatCannotRunRemovesTheNewServer(t *testing.T) {
	r := newRig(t)
	r.tester.err = errors.New("could not bring the client up")

	if _, err := r.eng.Move(context.Background(), MoveRequest{Trigger: TriggerCommand}); err == nil {
		t.Fatal("a test that could not run counted as success")
	}
	assertOnlyDeleted(t, r, "new-1")
	if r.ev.count("publish") != 0 {
		t.Error("published after the test failed to run")
	}
}

func TestAFailedPublishRemovesTheNewServer(t *testing.T) {
	r := newRig(t)
	r.pub.publishErr = errors.New("github is down")

	if _, err := r.eng.Move(context.Background(), MoveRequest{Trigger: TriggerCommand}); err == nil {
		t.Fatal("a failed publish counted as success")
	}
	assertOnlyDeleted(t, r, "new-1")
	if !r.cloud.has(r.cfg.InstanceID) {
		t.Error("the current server is gone although clients were never told about another")
	}
}

func TestACancelledMoveStillCleansUp(t *testing.T) {
	r := newRig(t)
	r.tester.report = failingReport()

	// The context is cancelled by the time the cleanup runs. The new server
	// must still be removed.
	ctx, cancel := context.WithCancel(context.Background())
	r.prov.reqs = nil
	origTest := r.tester
	_ = origTest
	cancel()

	_, err := r.eng.Move(ctx, MoveRequest{Trigger: TriggerCommand})
	if err == nil {
		t.Fatal("expected an error")
	}
	// Nothing was built under a context that was already dead, so nothing
	// exists to leak; the property is that no instance outlives the attempt.
	for _, i := range r.cloud.instances {
		if strings.HasPrefix(i.ID, "new-") {
			t.Errorf("instance %s outlived a cancelled move", i.ID)
		}
	}
}

// Whatever fails, the only server the engine may delete is the one it built.
func TestTheEngineNeverDeletesWhatItDidNotCreate(t *testing.T) {
	for name, setup := range map[string]func(*rig){
		"failed test":    func(r *rig) { r.tester.report = failingReport() },
		"failed publish": func(r *rig) { r.pub.publishErr = errors.New("x") },
		"test error":     func(r *rig) { r.tester.err = errors.New("x") },
	} {
		r := newRig(t)
		r.cloud.add(vultrInstance("someone-elses-1"))
		r.cloud.add(vultrInstance("someone-elses-2"))
		setup(r)

		_, _ = r.eng.Move(context.Background(), MoveRequest{Trigger: TriggerCommand})
		for _, id := range r.cloud.deletedIDs() {
			if id != "new-1" {
				t.Errorf("%s: deleted %q, which this attempt did not create", name, id)
			}
		}
		if !r.cloud.has("someone-elses-1") || !r.cloud.has("someone-elses-2") || !r.cloud.has(r.cfg.InstanceID) {
			t.Errorf("%s: an existing server disappeared", name)
		}
	}
}

func TestRegionChoice(t *testing.T) {
	r := newRig(t)
	r.cfg.Region = "sto"

	// Any other region by default, never the current one.
	for i := 0; i < 20; i++ {
		r.eng.Intn = func(n int) int { return i % n }
		got, err := r.eng.chooseRegion("")
		if err != nil {
			t.Fatal(err)
		}
		if got == "sto" {
			t.Fatalf("chose the current region")
		}
	}

	// An explicit region is honoured, including the current one.
	for _, want := range []string{"waw", "sto"} {
		if got, err := r.eng.chooseRegion(want); err != nil || got != want {
			t.Errorf("chooseRegion(%q) = %q, %v", want, got, err)
		}
	}
	// An unknown one is refused rather than guessed at.
	if _, err := r.eng.chooseRegion("moon"); err == nil {
		t.Error("an unknown region was accepted")
	}
}

func TestAMoveToARequestedRegionGoesThere(t *testing.T) {
	r := newRig(t)
	res := mustMove(t, r, MoveRequest{Trigger: TriggerCommand, Region: "fra"})
	if res.Region != "fra" || r.prov.reqs[0].Region != "fra" {
		t.Errorf("went to %s / %s, asked for fra", res.Region, r.prov.reqs[0].Region)
	}
}

func TestAnUnknownRegionFailsBeforeBuildingAnything(t *testing.T) {
	r := newRig(t)
	if _, err := r.eng.Move(context.Background(), MoveRequest{Trigger: TriggerCommand, Region: "moon"}); err == nil {
		t.Fatal("an unknown region was accepted")
	}
	if r.ev.count("provision") != 0 {
		t.Error("a server was built for an unknown region")
	}
}

// ---- retiring

func movedAndPublished(t *testing.T, r *rig) *MoveResult {
	t.Helper()
	return mustMove(t, r, MoveRequest{Trigger: TriggerCommand})
}

func TestRetireWaitsOutTheGraceThenDeletesThisServer(t *testing.T) {
	r := newRig(t)
	// Someone is connected, so there is no early exit.
	r.host.status = node.Status{UpdatedAt: r.clock.Now(),
		Peers: []node.PeerStatus{{LastHandshake: r.clock.Now()}}}
	res := movedAndPublished(t, r)
	published := r.clock.Now()

	// Keep the peer looking connected as time passes.
	if err := r.eng.Retire(context.Background(), res.AttemptID); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	if waited := r.clock.Now().Sub(published); waited < r.cfg.Policy.Grace() {
		t.Errorf("retired after %s, the grace period is %s", waited, r.cfg.Policy.Grace())
	}
	assertOnlyDeleted(t, r, r.cfg.InstanceID)
	if !r.cloud.has("new-1") {
		t.Error("the new server was deleted along with the old one")
	}
}

func TestRetireLeavesEarlyOnlyWhenIdleAndNotBeforeTheMinimum(t *testing.T) {
	r := newRig(t)
	// Nobody is connected.
	r.host.status = node.Status{UpdatedAt: r.clock.Now()}
	res := movedAndPublished(t, r)
	published := r.clock.Now()

	if err := r.eng.Retire(context.Background(), res.AttemptID); err != nil {
		t.Fatal(err)
	}
	waited := r.clock.Now().Sub(published)
	if waited < r.cfg.Policy.MinGrace() {
		t.Errorf("retired after %s, before the minimum of %s", waited, r.cfg.Policy.MinGrace())
	}
	if waited >= r.cfg.Policy.Grace() {
		t.Errorf("retired after %s although nobody was connected; the grace period is %s", waited, r.cfg.Policy.Grace())
	}
	assertOnlyDeleted(t, r, r.cfg.InstanceID)
}

func TestRetireRefusesAnAttemptThatWasNotPublished(t *testing.T) {
	r := newRig(t)
	if err := r.j.Begin(Attempt{ID: "a1", StartedAt: r.clock.Now().Unix(), State: StateVerified}); err != nil {
		t.Fatal(err)
	}
	if err := r.eng.Retire(context.Background(), "a1"); err == nil {
		t.Fatal("retired after an attempt that was never published")
	}
	if len(r.cloud.deletedIDs()) != 0 {
		t.Error("this server was deleted although the move never completed")
	}
	if err := r.eng.Retire(context.Background(), "missing"); err == nil {
		t.Error("retired after an attempt that does not exist")
	}
}

func TestADeleteThatFailsIsReportedAndRetried(t *testing.T) {
	r := newRig(t)
	r.host.status = node.Status{UpdatedAt: r.clock.Now()}
	res := movedAndPublished(t, r)

	r.cloud.deleteErr = errors.New("api timeout")
	if err := r.eng.Retire(context.Background(), res.AttemptID); err == nil {
		t.Fatal("a failed delete was reported as success")
	}
	if a := lastAttempt(r); a.State != StateRetiring {
		t.Errorf("state = %s, want retiring so that it is retried", a.State)
	}

	r.cloud.deleteErr = nil
	if err := r.eng.Retire(context.Background(), res.AttemptID); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	assertOnlyDeleted(t, r, r.cfg.InstanceID)
}

// ---- resuming after a restart

func TestResumeFinishesARetirementThatWasInProgress(t *testing.T) {
	r := newRig(t)
	r.host.status = node.Status{UpdatedAt: r.clock.Now()}
	movedAndPublished(t, r)
	r.clock.Advance(time.Hour) // the restart took a while

	if err := r.eng.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	assertOnlyDeleted(t, r, r.cfg.InstanceID)
}

func TestResumeRemovesASuccessorThatWasNeverPublished(t *testing.T) {
	r := newRig(t)
	r.cloud.add(vultrInstance("new-9"))
	r.pub.current = &fleet.ConfigBody{Endpoint: r.cfg.IP + ":1234"} // still points at this server
	if err := r.j.Begin(Attempt{ID: "a1", StartedAt: 1, State: StateProvisioned, InstanceID: "new-9", NewIP: "198.51.100.9"}); err != nil {
		t.Fatal(err)
	}

	if err := r.eng.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertOnlyDeleted(t, r, "new-9")
	if a := lastAttempt(r); a.State != StateFailed {
		t.Errorf("state = %s, want failed", a.State)
	}
	if !r.cloud.has(r.cfg.InstanceID) {
		t.Error("this server was deleted")
	}
}

// The dangerous case: the process died after publishing but before writing
// that down. The successor is live and must not be deleted.
func TestResumeKeepsASuccessorThatWasInFactPublished(t *testing.T) {
	r := newRig(t)
	r.host.status = node.Status{UpdatedAt: r.clock.Now()}
	r.cloud.add(vultrInstance("new-9"))
	r.pub.current = &fleet.ConfigBody{Endpoint: "198.51.100.9:4321"} // already points at the successor
	if err := r.j.Begin(Attempt{ID: "a1", StartedAt: 1, State: StateVerified, InstanceID: "new-9", NewIP: "198.51.100.9"}); err != nil {
		t.Fatal(err)
	}

	if err := r.eng.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range r.cloud.deletedIDs() {
		if id == "new-9" {
			t.Fatal("the live, published successor was deleted")
		}
	}
	// This server is the one that retires.
	assertOnlyDeleted(t, r, r.cfg.InstanceID)
}

// If it cannot tell, it must not guess.
func TestResumeLeavesEverythingAloneWhenItCannotReadTheGist(t *testing.T) {
	r := newRig(t)
	r.cloud.add(vultrInstance("new-9"))
	r.pub.currentErr = errors.New("github is unreachable")
	if err := r.j.Begin(Attempt{ID: "a1", StartedAt: 1, State: StateProvisioned, InstanceID: "new-9", NewIP: "198.51.100.9"}); err != nil {
		t.Fatal(err)
	}

	if err := r.eng.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.cloud.deletedIDs()) != 0 {
		t.Errorf("deleted %v without knowing whether it was published", r.cloud.deletedIDs())
	}
	if a := lastAttempt(r); a.State != StateProvisioned {
		t.Errorf("state changed to %s", a.State)
	}
}

func TestResumeMarksAnAttemptThatNeverBuiltAnythingFailed(t *testing.T) {
	r := newRig(t)
	if err := r.j.Begin(Attempt{ID: "a1", StartedAt: 1, State: StateStarted}); err != nil {
		t.Fatal(err)
	}
	if err := r.eng.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a := lastAttempt(r); a.State != StateFailed {
		t.Errorf("state = %s", a.State)
	}
	if len(r.cloud.deletedIDs()) != 0 {
		t.Error("deleted something although nothing had been built")
	}
}

func TestResumeWithNothingToResumeDoesNothing(t *testing.T) {
	r := newRig(t)
	if err := r.eng.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.ev.all()) != 0 {
		t.Errorf("did something with an empty journal: %v", r.ev.all())
	}
}

// ---- helpers

func lastAttempt(r *rig) Attempt {
	j := r.j.Read()
	a := j.Latest()
	if a == nil {
		r.t.Fatal("the journal has no attempts")
	}
	return *a
}

func assertOnlyDeleted(t *testing.T, r *rig, id string) {
	t.Helper()
	got := r.cloud.deletedIDs()
	if len(got) != 1 || got[0] != id {
		t.Errorf("deleted %v, want exactly [%s]", got, id)
	}
}

// The instance is written down the moment it exists, before the slow install,
// so that a process that dies during the install can still clean it up.
func TestTheNewServerIsRecordedBeforeTheInstallFinishes(t *testing.T) {
	r := newRig(t)
	r.prov.dieAfterCreate = errors.New("the process died here")

	if _, err := r.eng.Move(context.Background(), MoveRequest{Trigger: TriggerCommand}); err == nil {
		t.Fatal("expected the build to fail")
	}
	a := lastAttempt(r)
	if a.InstanceID != "new-1" {
		t.Fatalf("the journal does not know about the server that was created: %+v", a)
	}
}

// If the process dies mid-build, the server it created is still found and
// removed on the next start, as long as the gist does not point at it.
func TestResumeRemovesAServerThatWasCreatedButNeverFinished(t *testing.T) {
	r := newRig(t)
	r.cloud.add(vultrInstance("new-5"))
	r.pub.current = &fleet.ConfigBody{Endpoint: r.cfg.IP + ":1234"}
	// "started", but with the instance already recorded.
	if err := r.j.Begin(Attempt{ID: "a1", StartedAt: 1, State: StateStarted, InstanceID: "new-5", NewIP: "192.0.2.77"}); err != nil {
		t.Fatal(err)
	}

	if err := r.eng.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertOnlyDeleted(t, r, "new-5")
	if a := lastAttempt(r); a.State != StateFailed {
		t.Errorf("state = %s", a.State)
	}
}

// Found by killing the manager mid-build on the real account: the provider
// refused to delete the half-installed server ("locked"), and the journal
// still recorded it as removed. It must stay open and be retried.
func TestResumeDoesNotClaimARemovalThatFailed(t *testing.T) {
	r := newRig(t)
	r.cloud.add(vultrInstance("new-7"))
	r.pub.current = &fleet.ConfigBody{Endpoint: r.cfg.IP + ":1234"}
	if err := r.j.Begin(Attempt{ID: "a1", StartedAt: 1, State: StateStarted, InstanceID: "new-7"}); err != nil {
		t.Fatal(err)
	}

	r.cloud.deleteErr = errors.New("409 Conflict: Server is currently locked")
	if err := r.eng.Resume(context.Background()); err == nil {
		t.Fatal("a removal that failed was reported as done")
	}
	if a := lastAttempt(r); a.State == StateFailed || strings.Contains(a.Error, "was removed") {
		t.Fatalf("the journal claims the server is gone while it is still running: %+v", a)
	}
	if !r.cloud.has("new-7") {
		t.Fatal("setup: the server should still exist")
	}

	// Once the provider lets go of it, the next pass removes it for real.
	r.cloud.deleteErr = nil
	if err := r.eng.Resume(context.Background()); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	assertOnlyDeleted(t, r, "new-7")
	if a := lastAttempt(r); a.State != StateFailed {
		t.Errorf("state = %s after a successful removal", a.State)
	}
}

func TestAMoveThatCannotRemoveItsFailedSuccessorStaysOpenForARetry(t *testing.T) {
	r := newRig(t)
	r.tester.report = failingReport()
	r.cloud.deleteErr = errors.New("409 Conflict: Server is currently locked")

	_, err := r.eng.Move(context.Background(), MoveRequest{Trigger: TriggerCommand})
	if err == nil || !strings.Contains(err.Error(), "will be retried") {
		t.Fatalf("err = %v, want it to say the removal will be retried", err)
	}
	a := lastAttempt(r)
	if a.State == StateFailed {
		t.Fatal("the attempt was closed although the new server is still running")
	}
	if !strings.Contains(a.Error, "could not be removed") {
		t.Errorf("the journal does not say what is outstanding: %q", a.Error)
	}

	// The gist still points at this server, as it always does in real life
	// while a move is being attempted.
	r.pub.current = &fleet.ConfigBody{Endpoint: r.cfg.IP + ":1234"}
	r.cloud.deleteErr = nil
	if err := r.eng.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertOnlyDeleted(t, r, "new-1")
}

// ---- devices registered on the server

// deviceList is a registry file as the node writes it, with devices at the
// given host numbers.
func deviceList(hosts ...int) []byte {
	var devices []string
	for i, h := range hosts {
		id := bytes.Repeat([]byte{byte(i + 1)}, proto.DeviceIDSize)
		key := bytes.Repeat([]byte{byte(i + 0x40)}, proto.TunnelKeySize)
		devices = append(devices, fmt.Sprintf(
			`{"id":%q,"pub":%q,"address":"10.8.0.%d/32","enrolled_at":1,"last_seen":1}`,
			proto.Encode(id), proto.Encode(key), h))
	}
	return []byte(`{"version":1,"devices":[` + strings.Join(devices, ",") + `]}`)
}

func filesByPath(r *rig) map[string][]byte {
	out := map[string][]byte{}
	for _, f := range r.prov.files {
		out[f.Path] = f.Content
	}
	return out
}

// Devices must not have to register again after every move: that is what the
// list being handed on is for.
func TestTheSuccessorInheritsTheDevices(t *testing.T) {
	r := newRig(t)
	r.host.registry = deviceList(2, 3)
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	got, ok := filesByPath(r)[enroll.DefaultRegistryPath]
	if !ok {
		t.Fatalf("the device list was not passed on; files: %v", paths(r.prov.files))
	}
	if string(got) != string(r.host.registry) {
		t.Error("the successor was given a different list from the one the server holds")
	}
	for _, f := range r.prov.files {
		if f.Path == enroll.DefaultRegistryPath && f.Mode != "0600" {
			t.Errorf("the device list is written with mode %s; it holds every device's key", f.Mode)
		}
	}
}

func TestNoDevicesMeansNoFileToPassOn(t *testing.T) {
	r := newRig(t)
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})
	if _, ok := filesByPath(r)[enroll.DefaultRegistryPath]; ok {
		t.Error("an empty file was written for the successor")
	}
}

// A list that does not check out must not reach the new server, where it would
// stop the node starting. The devices in it register again by themselves.
func TestADeviceListThatDoesNotCheckOutIsNotPassedOn(t *testing.T) {
	for name, list := range map[string][]byte{
		"garbage":          []byte("not json"),
		"outside the pool": deviceList(2, 250),
		"wrong version":    []byte(`{"version":9,"devices":[]}`),
	} {
		r := newRig(t)
		r.host.registry = list
		mustMove(t, r, MoveRequest{Trigger: TriggerCommand}) // the move itself still succeeds
		if _, ok := filesByPath(r)[enroll.DefaultRegistryPath]; ok {
			t.Errorf("%s: passed on", name)
		}
	}
}

// Without this a new server would have no way to take on a device.
func TestTheNewServerIsSetUpAndPublishedForRegistration(t *testing.T) {
	r := newRig(t)
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	req := r.prov.reqs[len(r.prov.reqs)-1]
	if req.Node.Enroll == nil {
		t.Fatal("the new server is not told to register devices")
	}
	if req.Node.Enroll.ContentKey != r.cfg.ContentKey {
		t.Error("the new server was given a different content key, so it could not read a request")
	}

	body := r.pub.published[len(r.pub.published)-1]
	if body.Enroll == nil {
		t.Fatal("the published configuration does not say how to register")
	}
	if body.Shared != nil {
		t.Error("the published configuration still carries a key shared by everyone")
	}
}

// A server that cannot take on a device must not be published, so the test it
// has to pass includes doing exactly that, with what a device is given.
func TestTheNewServerIsTestedForRegistration(t *testing.T) {
	r := newRig(t)
	mustMove(t, r, MoveRequest{Trigger: TriggerCommand})

	reg := r.tester.target.Registration
	if reg == nil {
		t.Fatal("the test does not try to register a device")
	}
	body := r.pub.published[len(r.pub.published)-1]
	if reg.PeerPrivate != body.Enroll.PrivateKey {
		t.Error("the test registers with a different enrollment key from the one published")
	}
	if proto.Encode(reg.ContentKey) != r.cfg.ContentKey {
		t.Error("the test seals its request with a different content key")
	}
	if reg.Endpoint.String() != body.Endpoint {
		t.Errorf("the test registers at %s, devices will use %s", reg.Endpoint, body.Endpoint)
	}
}
