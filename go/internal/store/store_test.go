package store

import (
	"strings"
	"testing"
	"time"

	"awgrotator/internal/proto"
)

func TestNewGeneratesDistinctMaterial(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seen := map[string]string{}
	for name, v := range map[string]string{
		"root":    s.Keys.RootSeed,
		"backup":  s.Keys.BackupSeed,
		"server":  s.Keys.ServerSeed,
		"content": s.Keys.ContentKey,
	} {
		if v == "" {
			t.Errorf("%s key is empty", name)
		}
		if other, dup := seen[v]; dup {
			t.Errorf("%s key is identical to %s", name, other)
		}
		seen[v] = name
	}
	if s.Epoch != 1 {
		t.Errorf("Epoch = %d, want 1", s.Epoch)
	}
	if s.Seq != 0 {
		t.Errorf("Seq = %d, want 0", s.Seq)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	if Exists(root) {
		t.Fatal("a fresh directory reports existing state")
	}

	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.GistID = "deadbeef"
	s.GistOwner = "someone"
	s.Server = Server{ID: "abc", Region: "sto", IP: "203.0.113.9"}

	if err := s.Save(root); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !Exists(root) {
		t.Fatal("state was not written")
	}

	got, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Keys != s.Keys {
		t.Error("keys did not survive the round trip")
	}
	if got.GistID != "deadbeef" || got.GistOwner != "someone" {
		t.Errorf("gist = %q/%q", got.GistID, got.GistOwner)
	}
	if got.Server != s.Server {
		t.Errorf("server = %+v", got.Server)
	}
	if got.SSHPrivate != s.SSHPrivate {
		t.Error("ssh key did not survive the round trip")
	}
}

func TestLoadWithoutStateExplainsItself(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil {
		t.Fatal("loading an empty directory succeeded")
	}
	if !strings.Contains(err.Error(), "awgctl init") {
		t.Errorf("error does not point at init: %v", err)
	}
}

func TestSSHKeyIsUsable(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.HasPrefix(s.SSHPublic, "ssh-ed25519 ") {
		t.Errorf("public key line = %q", s.SSHPublic)
	}
	signer, err := s.SSHSigner()
	if err != nil {
		t.Fatalf("SSHSigner: %v", err)
	}
	if got := signer.PublicKey().Type(); got != "ssh-ed25519" {
		t.Errorf("key type = %q", got)
	}
}

// A certificate issued by the state must verify against the trust bundle the
// apps will be built with, and a document signed by the server key must open
// through it.
func TestCertAndDocVerifyAgainstTrust(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	trust, err := s.Trust()
	if err != nil {
		t.Fatalf("Trust: %v", err)
	}

	now := time.Now()
	cert, err := s.NewCert(30*24*time.Hour, now)
	if err != nil {
		t.Fatalf("NewCert: %v", err)
	}
	if err := trust.VerifyCert(&cert, now, proto.State{}); err != nil {
		t.Fatalf("VerifyCert: %v", err)
	}

	serverKey, err := s.ServerKey()
	if err != nil {
		t.Fatalf("ServerKey: %v", err)
	}
	contentKey, err := s.ContentKey()
	if err != nil {
		t.Fatalf("ContentKey: %v", err)
	}

	doc := &proto.Doc{
		Kind:      proto.KindConfig,
		Seq:       s.NextSeq(),
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(),
		Cert:      cert,
		Body:      `{"endpoint":"203.0.113.9:51820"}`,
	}
	blob, err := proto.SealDoc(doc, serverKey, contentKey)
	if err != nil {
		t.Fatalf("SealDoc: %v", err)
	}

	got, next, err := trust.OpenDoc(blob, proto.KindConfig, now, proto.State{})
	if err != nil {
		t.Fatalf("OpenDoc: %v", err)
	}
	if got.Body != doc.Body {
		t.Errorf("body = %q", got.Body)
	}
	if next.LastSeq != 1 {
		t.Errorf("LastSeq = %d, want 1", next.LastSeq)
	}
}

func TestNextSeqIncrements(t *testing.T) {
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for want := uint64(1); want <= 3; want++ {
		if got := s.NextSeq(); got != want {
			t.Fatalf("NextSeq = %d, want %d", got, want)
		}
	}
	if s.Seq != 3 {
		t.Errorf("Seq = %d, want 3", s.Seq)
	}
}
