package proto

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Fixed material so every test (and every vector) is reproducible.
var (
	rootSeed    = repeat(0x01)
	backupSeed  = repeat(0x02)
	serverSeed  = repeat(0x03)
	strangeSeed = repeat(0x07)
	testContent = repeat(0x04)

	testIat = int64(1762400000)
	testExp = testIat + 60*24*3600
	testNow = time.Unix(testIat+3600, 0)
)

func repeat(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

type fixture struct {
	root, backup, server ed25519.PrivateKey
	trust                Trust
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{
		root:   ed25519.NewKeyFromSeed(rootSeed),
		backup: ed25519.NewKeyFromSeed(backupSeed),
		server: ed25519.NewKeyFromSeed(serverSeed),
	}
	f.trust = Trust{
		RootPub:    Encode(PublicOf(f.root)),
		BackupPub:  Encode(PublicOf(f.backup)),
		ContentKey: Encode(testContent),
	}
	return f
}

// cert builds a certificate for the server key at the given epoch.
func (f fixture) cert(t *testing.T, epoch uint32, issuer string) Cert {
	t.Helper()
	c := Cert{
		Epoch:     epoch,
		NotBefore: testIat,
		NotAfter:  testExp,
		ServerPub: Encode(PublicOf(f.server)),
	}
	priv := f.root
	if issuer == IssuerBackup {
		priv = f.backup
	}
	if err := c.Sign(issuer, priv); err != nil {
		t.Fatalf("sign cert: %v", err)
	}
	return c
}

func (f fixture) doc(t *testing.T, kind string, seq uint64, body string, c Cert) *Doc {
	t.Helper()
	return &Doc{
		Kind:      kind,
		Seq:       seq,
		IssuedAt:  testIat,
		ExpiresAt: testExp,
		Cert:      c,
		Body:      body,
	}
}

const testBody = `{"endpoint":"203.0.113.9:51820","region":"sto"}`

func TestConfigRoundTrip(t *testing.T) {
	f := newFixture(t)
	d := f.doc(t, KindConfig, 7, testBody, f.cert(t, 1, IssuerRoot))

	blob, err := SealDoc(d, f.server, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	got, next, err := f.trust.OpenDoc(blob, KindConfig, testNow, State{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got.Body != testBody {
		t.Errorf("body = %q, want %q", got.Body, testBody)
	}
	if next.LastSeq != 7 {
		t.Errorf("next.LastSeq = %d, want 7", next.LastSeq)
	}
}

func TestBackupKeyMayCertify(t *testing.T) {
	f := newFixture(t)
	d := f.doc(t, KindConfig, 1, testBody, f.cert(t, 1, IssuerBackup))
	blob, err := SealDoc(d, f.server, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, _, err := f.trust.OpenDoc(blob, KindConfig, testNow, State{}); err != nil {
		t.Fatalf("open: %v", err)
	}
}

func TestWrongContentKeyIsRejected(t *testing.T) {
	f := newFixture(t)
	d := f.doc(t, KindConfig, 1, testBody, f.cert(t, 1, IssuerRoot))
	blob, err := SealDoc(d, f.server, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	bad := f.trust
	bad.ContentKey = Encode(repeat(0xAA))
	if _, _, err := bad.OpenDoc(blob, KindConfig, testNow, State{}); err == nil {
		t.Fatal("a document opened with the wrong content key")
	}
}

// A body swapped after signing must not verify. This is the attack the whole
// scheme exists to stop: someone with the app (and therefore the content key)
// pointing the app at their own server.
func TestTamperedBodyIsRejected(t *testing.T) {
	f := newFixture(t)
	d := f.doc(t, KindConfig, 1, testBody, f.cert(t, 1, IssuerRoot))
	if err := d.Sign(f.server); err != nil {
		t.Fatalf("sign: %v", err)
	}
	d.Body = `{"endpoint":"198.51.100.4:51820","region":"sto"}`

	plain, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	blob, err := Seal(testContent, plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	_, _, err = f.trust.OpenDoc(blob, KindConfig, testNow, State{})
	if err == nil {
		t.Fatal("a tampered body verified")
	}
	if !strings.Contains(err.Error(), "not signed by the key") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUncertifiedServerKeyIsRejected(t *testing.T) {
	f := newFixture(t)
	stranger := ed25519.NewKeyFromSeed(strangeSeed)

	// A certificate naming the stranger's key, signed by nobody we trust.
	c := Cert{
		Epoch:     1,
		NotBefore: testIat,
		NotAfter:  testExp,
		ServerPub: Encode(PublicOf(stranger)),
	}
	if err := c.Sign(IssuerRoot, stranger); err != nil {
		t.Fatalf("sign cert: %v", err)
	}

	d := f.doc(t, KindConfig, 1, testBody, c)
	blob, err := SealDoc(d, stranger, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, _, err := f.trust.OpenDoc(blob, KindConfig, testNow, State{}); err == nil {
		t.Fatal("a self-signed certificate was accepted")
	}
}

func TestOldSeqIsRejected(t *testing.T) {
	f := newFixture(t)
	d := f.doc(t, KindConfig, 5, testBody, f.cert(t, 1, IssuerRoot))
	blob, err := SealDoc(d, f.server, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	st := State{LastSeq: 5}
	if _, _, err := f.trust.OpenDoc(blob, KindConfig, testNow, st); err == nil {
		t.Fatal("a replayed configuration was accepted")
	}
}

func TestStatusMayRepeatSeq(t *testing.T) {
	f := newFixture(t)
	d := f.doc(t, KindStatus, 1, `{"age_days":12}`, f.cert(t, 1, IssuerRoot))
	blob, err := SealDoc(d, f.server, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	st := State{LastSeq: 9}
	got, next, err := f.trust.OpenDoc(blob, KindStatus, testNow, st)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got.Kind != KindStatus {
		t.Errorf("kind = %q", got.Kind)
	}
	if next.LastSeq != 9 {
		t.Errorf("a status report moved LastSeq to %d", next.LastSeq)
	}
}

// A status document must not be accepted where a configuration is expected.
func TestKindConfusionIsRejected(t *testing.T) {
	f := newFixture(t)
	d := f.doc(t, KindStatus, 3, testBody, f.cert(t, 1, IssuerRoot))
	blob, err := SealDoc(d, f.server, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, _, err := f.trust.OpenDoc(blob, KindConfig, testNow, State{}); err == nil {
		t.Fatal("a status report was accepted as a configuration")
	}
}

func TestRevocationCutsOffOldEpoch(t *testing.T) {
	f := newFixture(t)

	r := &Revocation{MinEpoch: 4, IssuedAt: testIat}
	blob, err := SealRevocation(r, IssuerBackup, f.backup, testContent)
	if err != nil {
		t.Fatalf("seal revocation: %v", err)
	}

	_, st, err := f.trust.OpenRevocation(blob, testNow, State{})
	if err != nil {
		t.Fatalf("open revocation: %v", err)
	}
	if st.MinEpoch != 4 {
		t.Fatalf("MinEpoch = %d, want 4", st.MinEpoch)
	}

	// Epoch 3 is now dead, epoch 4 still works.
	for _, tc := range []struct {
		epoch uint32
		ok    bool
	}{{3, false}, {4, true}} {
		d := f.doc(t, KindConfig, 1, testBody, f.cert(t, tc.epoch, IssuerRoot))
		cfg, err := SealDoc(d, f.server, testContent)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		_, _, err = f.trust.OpenDoc(cfg, KindConfig, testNow, st)
		if tc.ok && err != nil {
			t.Errorf("epoch %d: %v", tc.epoch, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("epoch %d was accepted after revocation", tc.epoch)
		}
	}
}

func TestRevocationMustRaiseEpoch(t *testing.T) {
	f := newFixture(t)
	r := &Revocation{MinEpoch: 2, IssuedAt: testIat}
	blob, err := SealRevocation(r, IssuerRoot, f.root, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, _, err := f.trust.OpenRevocation(blob, testNow, State{MinEpoch: 5}); err == nil {
		t.Fatal("a revocation lowered the minimum epoch")
	}
}

func TestExpiredCertIsRejected(t *testing.T) {
	f := newFixture(t)
	d := f.doc(t, KindConfig, 1, testBody, f.cert(t, 1, IssuerRoot))
	blob, err := SealDoc(d, f.server, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	late := time.Unix(testExp+2*3600, 0)
	if _, _, err := f.trust.OpenDoc(blob, KindConfig, late, State{}); err == nil {
		t.Fatal("an expired certificate was accepted")
	}
}

func TestCommandRoundTripAndFreshness(t *testing.T) {
	f := newFixture(t)
	id, err := NewCommandID()
	if err != nil {
		t.Fatalf("command id: %v", err)
	}

	c := &Command{ID: id, IssuedAt: testIat, Action: ActionMoveRegion, Arg: "waw"}
	blob, err := SealCommand(c, IssuerRoot, f.root, testContent)
	if err != nil {
		t.Fatalf("seal command: %v", err)
	}

	got, err := f.trust.OpenCommand(blob, time.Unix(testIat+60, 0))
	if err != nil {
		t.Fatalf("open command: %v", err)
	}
	if got.Action != ActionMoveRegion || got.Arg != "waw" {
		t.Errorf("got action %q arg %q", got.Action, got.Arg)
	}

	// Same command, replayed an hour later.
	if _, err := f.trust.OpenCommand(blob, time.Unix(testIat+3600, 0)); err == nil {
		t.Fatal("a stale command was accepted")
	}
}

func TestCommandFromUntrustedKeyIsRejected(t *testing.T) {
	f := newFixture(t)
	stranger := ed25519.NewKeyFromSeed(strangeSeed)
	id, err := NewCommandID()
	if err != nil {
		t.Fatalf("command id: %v", err)
	}

	c := &Command{ID: id, IssuedAt: testIat, Action: ActionMove}
	blob, err := SealCommand(c, IssuerRoot, stranger, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := f.trust.OpenCommand(blob, time.Unix(testIat+60, 0)); err == nil {
		t.Fatal("a command signed by an unknown key was accepted")
	}
}

func TestSignRefusesMismatchedServerKey(t *testing.T) {
	f := newFixture(t)
	d := f.doc(t, KindConfig, 1, testBody, f.cert(t, 1, IssuerRoot))
	stranger := ed25519.NewKeyFromSeed(strangeSeed)
	if err := d.Sign(stranger); err == nil {
		t.Fatal("signing succeeded with a key the certificate does not name")
	}
}

func TestEnvelopeNonceVaries(t *testing.T) {
	a, err := Seal(testContent, []byte("same plaintext"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	b, err := Seal(testContent, []byte("same plaintext"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if a == b {
		t.Fatal("two seals of the same plaintext produced the same blob")
	}
}

func TestUnknownActionAndIssuerRejected(t *testing.T) {
	f := newFixture(t)
	id, err := NewCommandID()
	if err != nil {
		t.Fatalf("command id: %v", err)
	}
	if err := (&Command{ID: id, IssuedAt: testIat, Action: "wipe"}).Sign(IssuerRoot, f.root); err == nil {
		t.Error("an unknown action was signed")
	}
	if err := (&Command{ID: id, IssuedAt: testIat, Action: ActionMove}).Sign("admin", f.root); err == nil {
		t.Error("an unknown issuer was signed")
	}
}
