package lifecycle

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"awgrotator/internal/proto"
)

var t0 = time.Unix(1_800_000_000, 0)

// testKeys holds the operator's keys for a test.
type testKeys struct {
	root, backup ed25519.PrivateKey
	content      []byte
}

func newTestKeys(t *testing.T) testKeys {
	t.Helper()
	root, err := proto.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	backup, err := proto.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	content, err := proto.GenerateContentKey()
	if err != nil {
		t.Fatal(err)
	}
	return testKeys{root: root, backup: backup, content: content}
}

// testConfig returns a valid manager configuration with real keys.
func testConfig(t *testing.T, keys testKeys) *Config {
	t.Helper()

	server, err := proto.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	cert := proto.Cert{
		Epoch:     1,
		NotBefore: t0.Add(-time.Hour).Unix(),
		NotAfter:  t0.Add(365 * 24 * time.Hour).Unix(),
		ServerPub: proto.Encode(proto.PublicOf(server)),
	}
	if err := cert.Sign(proto.IssuerRoot, keys.root); err != nil {
		t.Fatal(err)
	}

	_, sshPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(sshPriv, "test")
	if err != nil {
		t.Fatal(err)
	}

	return &Config{
		Version:     ConfigVersion,
		InstanceID:  "self-instance",
		IP:          "203.0.113.10",
		Region:      "sto",
		CreatedAt:   t0.Unix(),
		Generation:  1,
		VultrKey:    "vultr-key",
		GitHubToken: "github-token",
		GistID:      "gist-id",
		ServerSeed:  proto.Encode(server.Seed()),
		Cert:        cert,
		ContentKey:  proto.Encode(keys.content),
		RootPub:     proto.Encode(proto.PublicOf(keys.root)),
		BackupPub:   proto.Encode(proto.PublicOf(keys.backup)),
		SSHPrivate:  string(pem.EncodeToMemory(block)),
		SSHKeyID:    "ssh-key-id",
		Plan:        "vc2-1c-1gb",
		OsID:        2625,
		Tag:         "awg-rotator",
		Regions:     []string{"sto", "waw", "fra", "ams"},
		Policy:      DefaultPolicy(),
	}
}

func TestAValidConfigPasses(t *testing.T) {
	cfg := testConfig(t, newTestKeys(t))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := cfg.ServerKey(); err != nil {
		t.Errorf("ServerKey: %v", err)
	}
	if _, err := cfg.SSHSigner(); err != nil {
		t.Errorf("SSHSigner: %v", err)
	}
	if _, err := cfg.ContentKeyBytes(); err != nil {
		t.Errorf("ContentKeyBytes: %v", err)
	}
}

func TestConfigValidationCatchesWhatWouldFailLater(t *testing.T) {
	keys := newTestKeys(t)
	cases := map[string]func(*Config){
		"wrong version":        func(c *Config) { c.Version = 9 },
		"no instance":          func(c *Config) { c.InstanceID = "" },
		"no vultr key":         func(c *Config) { c.VultrKey = "" },
		"no github token":      func(c *Config) { c.GitHubToken = "" },
		"no gist":              func(c *Config) { c.GistID = "" },
		"no ssh key id":        func(c *Config) { c.SSHKeyID = "" },
		"no os":                func(c *Config) { c.OsID = 0 },
		"one region":           func(c *Config) { c.Regions = []string{"sto"} },
		"no generation":        func(c *Config) { c.Generation = 0 },
		"no creation time":     func(c *Config) { c.CreatedAt = 0 },
		"garbage seed":         func(c *Config) { c.ServerSeed = "!!!" },
		"garbage ssh key":      func(c *Config) { c.SSHPrivate = "not a key" },
		"policy ceiling":       func(c *Config) { c.Policy.MaxServers = 50 },
		"cert for another key": func(c *Config) { c.Cert.ServerPub = proto.Encode(make([]byte, 32)) },
	}
	for name, mutate := range cases {
		cfg := testConfig(t, keys)
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigRoundTripKeepsEverythingAndIsPrivate(t *testing.T) {
	cfg := testConfig(t, newTestKeys(t))
	path := filepath.Join(t.TempDir(), "sub", "manager.json")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.InstanceID != cfg.InstanceID || got.ServerSeed != cfg.ServerSeed || got.Cert != cfg.Cert {
		t.Error("the configuration changed in the round trip")
	}
	if got.Policy != cfg.Policy {
		t.Errorf("policy changed: %+v vs %+v", got.Policy, cfg.Policy)
	}
}

func TestPolicyDefaultsAreSafeAndAutomaticMovesStartOff(t *testing.T) {
	p := DefaultPolicy()
	if p.AutoAge || p.AutoBlock {
		t.Error("automatic moves are on by default")
	}
	if err := p.Normalize(); err != nil {
		t.Fatalf("the default policy is not valid: %v", err)
	}
	if p.MaxServers > 3 || p.MaxMovesPerDay > 3 {
		t.Errorf("default ceilings are too high: %+v", p)
	}

	empty := Policy{}
	if err := empty.Normalize(); err != nil {
		t.Fatal(err)
	}
	if empty.MaxServers != p.MaxServers || empty.GraceMinutes != p.GraceMinutes {
		t.Errorf("an empty policy was not filled in: %+v", empty)
	}
}

func TestPolicyRefusesDangerousValues(t *testing.T) {
	for name, mutate := range map[string]func(*Policy){
		"a single server":         func(p *Policy) { p.MaxServers = 1 },
		"too many servers":        func(p *Policy) { p.MaxServers = 7 },
		"too many moves a day":    func(p *Policy) { p.MaxMovesPerDay = 13 },
		"negative jitter":         func(p *Policy) { p.AgeJitterHours = -1 },
		"minimum above the grace": func(p *Policy) { p.MinGraceMinutes = 99; p.GraceMinutes = 10 },
	} {
		p := DefaultPolicy()
		mutate(&p)
		if err := p.Normalize(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func inputs(now time.Time) Inputs {
	return Inputs{Now: now, CreatedAt: t0, Health: Healthy}
}

func TestNothingMovesWhenAutomaticMovesAreOff(t *testing.T) {
	p := DefaultPolicy()
	in := inputs(t0.Add(365 * 24 * time.Hour)) // a year old
	in.Health = Blocked

	d := Decide(p, in)
	if d.Move {
		t.Fatalf("a server moved with automatic moves off: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "off") {
		t.Errorf("the reason does not say why: %q", d.Reason)
	}
}

func TestAnOperatorCommandAlwaysGoesAhead(t *testing.T) {
	p := DefaultPolicy()
	in := inputs(t0.Add(time.Minute))
	// Even straight after a failed attempt and past the daily limit.
	in.Attempts = []time.Time{in.Now.Add(-time.Minute), in.Now.Add(-2 * time.Minute), in.Now.Add(-3 * time.Minute)}
	in.Command = &MoveRequest{Trigger: TriggerCommand, Region: "waw"}

	d := Decide(p, in)
	if !d.Move || d.Request.Trigger != TriggerCommand || d.Request.Region != "waw" {
		t.Errorf("an operator command did not go ahead: %+v", d)
	}
}

func TestAgeTrigger(t *testing.T) {
	p := DefaultPolicy()
	p.AutoAge = true

	young := Decide(p, inputs(t0.Add(10*24*time.Hour)))
	if young.Move {
		t.Errorf("a young server moved: %s", young.Reason)
	}

	old := Decide(p, inputs(t0.Add(p.MaxAge()+time.Hour)))
	if !old.Move || old.Request.Trigger != TriggerAge {
		t.Errorf("an old server did not move: %+v", old)
	}

	// The jitter delays it.
	in := inputs(t0.Add(p.MaxAge() + time.Hour))
	in.AgeJitter = 48 * time.Hour
	if Decide(p, in).Move {
		t.Error("the jitter was ignored")
	}
}

func TestBlockedAndBrokenTriggersNeedTheirSwitch(t *testing.T) {
	p := DefaultPolicy()
	p.AutoAge = true // the other switch on must not enable these

	for _, v := range []Verdict{Blocked, Broken} {
		in := inputs(t0.Add(time.Hour))
		in.Health = v
		if Decide(p, in).Move {
			t.Errorf("%s moved with AutoBlock off", v)
		}
	}

	p.AutoBlock = true
	in := inputs(t0.Add(time.Hour))
	in.Health = Blocked
	if d := Decide(p, in); !d.Move || d.Request.Trigger != TriggerBlocked {
		t.Errorf("a blocked server did not move: %+v", d)
	}
	in.Health = Broken
	if d := Decide(p, in); !d.Move || d.Request.Trigger != TriggerBroken {
		t.Errorf("a broken server did not move: %+v", d)
	}
	in.Health = Healthy
	if Decide(p, in).Move {
		t.Error("a healthy server moved")
	}
}

// The limits that stop a recurring fault from becoming a loop of paid servers.
func TestAutomaticMovesAreRateLimited(t *testing.T) {
	p := DefaultPolicy()
	p.AutoBlock = true

	in := inputs(t0.Add(48 * time.Hour))
	in.Health = Blocked

	// Too soon after the last attempt.
	in.Attempts = []time.Time{in.Now.Add(-time.Hour)}
	if d := Decide(p, in); d.Move || !strings.Contains(d.Reason, "too recent") {
		t.Errorf("a move straight after an attempt: %+v", d)
	}

	// Long enough since the last one.
	in.Attempts = []time.Time{in.Now.Add(-7 * time.Hour)}
	if !Decide(p, in).Move {
		t.Error("a move was refused although the pause had passed")
	}

	// The daily limit, with the attempts spaced beyond the minimum interval.
	p.MinMoveMinutes = 1
	p.MaxMovesPerDay = 3
	in.Attempts = []time.Time{in.Now.Add(-2 * time.Hour), in.Now.Add(-4 * time.Hour), in.Now.Add(-6 * time.Hour)}
	if d := Decide(p, in); d.Move || !strings.Contains(d.Reason, "today") {
		t.Errorf("the daily limit did not hold: %+v", d)
	}

	// Attempts older than a day do not count.
	in.Attempts = []time.Time{in.Now.Add(-30 * time.Hour), in.Now.Add(-40 * time.Hour), in.Now.Add(-50 * time.Hour)}
	if !Decide(p, in).Move {
		t.Error("old attempts counted against today's limit")
	}
}

func TestNextAutoMove(t *testing.T) {
	p := DefaultPolicy()
	if _, ok := NextAutoMove(p, t0, 0); ok {
		t.Error("a date was given with age moves off")
	}
	p.AutoAge = true
	at, ok := NextAutoMove(p, t0, 24*time.Hour)
	if !ok || !at.Equal(t0.Add(p.MaxAge()+24*time.Hour)) {
		t.Errorf("NextAutoMove = %v, %v", at, ok)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name            string
		target, control Reach
		want            Round
	}{
		{"all reach", Reach{10, 10}, Reach{6, 6}, Good},
		{"nearly none from the target", Reach{1, 10}, Reach{6, 6}, Bad},
		{"none from the target", Reach{0, 10}, Reach{6, 6}, Bad},
		{"half from the target", Reach{5, 10}, Reach{6, 6}, Good},
		// If the control cannot reach it either, nothing is known.
		{"control fails too", Reach{0, 10}, Reach{1, 6}, Inconclusive},
		{"no target probes", Reach{0, 0}, Reach{6, 6}, Inconclusive},
		{"no control probes", Reach{0, 10}, Reach{0, 0}, Inconclusive},
	} {
		if got := Classify(tc.target, tc.control); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTrackerNeedsSeveralBadRoundsAndOneGoodClearsIt(t *testing.T) {
	tr := NewTracker(3)
	for i := 0; i < 2; i++ {
		tr.External(Bad)
	}
	if tr.Verdict() != Healthy {
		t.Errorf("two bad rounds gave %s, want healthy", tr.Verdict())
	}
	tr.External(Bad)
	if tr.Verdict() != Blocked {
		t.Errorf("three bad rounds gave %s, want blocked", tr.Verdict())
	}
	tr.External(Good)
	if tr.Verdict() != Healthy || tr.BadRounds() != 0 {
		t.Errorf("a good round did not clear it: %s, %d", tr.Verdict(), tr.BadRounds())
	}
}

func TestInconclusiveRoundsChangeNothing(t *testing.T) {
	tr := NewTracker(3)
	tr.External(Bad)
	tr.External(Bad)
	for i := 0; i < 10; i++ {
		tr.External(Inconclusive)
	}
	if tr.BadRounds() != 2 {
		t.Errorf("inconclusive rounds changed the count to %d", tr.BadRounds())
	}
	tr.External(Bad)
	if tr.Verdict() != Blocked {
		t.Error("an inconclusive stretch hid a real run of bad rounds")
	}
}

func TestBrokenOutranksBlocked(t *testing.T) {
	tr := NewTracker(1)
	tr.External(Bad)
	for i := 0; i < localFailuresForBroken; i++ {
		tr.Local(false)
	}
	if tr.Verdict() != Broken {
		t.Errorf("verdict = %s, want broken", tr.Verdict())
	}
	tr.Local(true)
	if tr.Verdict() != Blocked {
		t.Errorf("after recovering locally verdict = %s, want blocked", tr.Verdict())
	}
}

func TestOneLocalFailureIsNotBroken(t *testing.T) {
	tr := NewTracker(3)
	tr.Local(false)
	tr.Local(false)
	if tr.Verdict() != Healthy {
		t.Errorf("two local failures gave %s", tr.Verdict())
	}
	tr.Local(true)
	tr.Local(false)
	if tr.Verdict() != Healthy {
		t.Error("a success did not reset the failure count")
	}
}

func TestJournalPersistsAndRollsBackOnAFailedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	js, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Begin(Attempt{ID: "a1", StartedAt: t0.Unix(), Trigger: TriggerCommand, State: StateStarted}); err != nil {
		t.Fatal(err)
	}
	if err := js.Step("a1", func(a *Attempt) { a.State = StatePublished; a.InstanceID = "new" }); err != nil {
		t.Fatal(err)
	}
	if err := js.Update(func(j *Journal) { j.MarkExecuted("cmd-1"); j.LastSeq = 7 }); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Read()
	if len(got.Attempts) != 1 || got.Attempts[0].State != StatePublished || got.Attempts[0].InstanceID != "new" {
		t.Errorf("attempts = %+v", got.Attempts)
	}
	if !got.WasExecuted("cmd-1") || got.LastSeq != 7 {
		t.Errorf("journal = %+v", got)
	}

	// Make saving impossible by putting a directory where the file goes.
	bad, err := OpenJournal(filepath.Join(t.TempDir(), "j", "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.Update(func(j *Journal) { j.LastSeq = 1 }); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(filepath.Dir(bad.path), "journal.json.tmp")
	if err := mkdir(blocker); err != nil {
		t.Fatal(err)
	}
	if err := bad.Update(func(j *Journal) { j.LastSeq = 99 }); err == nil {
		t.Fatal("a save that could not happen was reported as success")
	}
	if bad.Read().LastSeq != 1 {
		t.Errorf("memory is ahead of the disk after a failed save: LastSeq=%d", bad.Read().LastSeq)
	}
}

func TestJournalHistoryIsBounded(t *testing.T) {
	js, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxAttempts+10; i++ {
		if err := js.Begin(Attempt{ID: string(rune('a' + i%26)), StartedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(js.Read().Attempts); got != maxAttempts {
		t.Errorf("%d attempts kept, want %d", got, maxAttempts)
	}
	// The newest are the ones kept.
	snapshot := js.Read()
	if latest := snapshot.Latest(); latest == nil || latest.StartedAt != int64(maxAttempts+9) {
		t.Errorf("latest = %+v", latest)
	}

	for i := 0; i < maxExecuted+5; i++ {
		id := string(rune(0x400 + i))
		_ = js.Update(func(j *Journal) { j.MarkExecuted(id) })
	}
	if got := len(js.Read().Executed); got != maxExecuted {
		t.Errorf("%d executed ids kept, want %d", got, maxExecuted)
	}
}

func TestJournalStepOnAMissingAttemptIsAnError(t *testing.T) {
	js, err := OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Step("nope", func(*Attempt) {}); err == nil {
		t.Error("changing a missing attempt succeeded")
	}
}

func TestJournalRejectsACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	if err := writeText(path, "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(path); err == nil {
		t.Error("a corrupt journal was accepted")
	}
}

func TestCertDaysLeft(t *testing.T) {
	cfg := testConfig(t, newTestKeys(t))
	if got := cfg.CertDaysLeft(t0); got < 364 || got > 366 {
		t.Errorf("CertDaysLeft = %d for a year-long certificate", got)
	}
	if got := cfg.CertDaysLeft(t0.Add(400 * 24 * time.Hour)); got != 0 {
		t.Errorf("an expired certificate has %d days left", got)
	}
}
