package proto

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Run with -update to rewrite spec/vectors.json from this implementation.
var update = flag.Bool("update", false, "rewrite spec/vectors.json")

const vectorsPath = "../../../spec/vectors.json"

// Vectors is the contract between this package and the Dart port in the apps.
// Both must produce exactly these signed byte strings and signatures.
type Vectors struct {
	Note string `json:"note"`
	Keys struct {
		RootSeed   string `json:"root_seed"`
		RootPub    string `json:"root_pub"`
		BackupSeed string `json:"backup_seed"`
		BackupPub  string `json:"backup_pub"`
		ServerSeed string `json:"server_seed"`
		ServerPub  string `json:"server_pub"`
		ContentKey string `json:"content_key"`
	} `json:"keys"`
	Cases    []VectorCase `json:"cases"`
	Envelope struct {
		Nonce     string `json:"nonce"`
		Plaintext string `json:"plaintext_b64"`
		Blob      string `json:"blob"`
	} `json:"envelope"`

	// Enroll pins down the two registration messages (spec section 4.5): the
	// plaintext JSON a port must be able to read, and the envelope around it.
	// A port seals its own with a random nonce, so what it must reproduce is
	// the fields, not the blob.
	Enroll struct {
		Now           int64  `json:"now"`
		DeviceID      string `json:"device_id"`
		PublicKey     string `json:"public_key"`
		RequestNonce  string `json:"request_nonce"`
		Name          string `json:"name"`
		Address       string `json:"address"`
		EnvelopeNonce string `json:"envelope_nonce"`
		RequestPlain  string `json:"request_plain"`
		RequestBlob   string `json:"request_blob"`
		ReplyPlain    string `json:"reply_plain"`
		ReplyBlob     string `json:"reply_blob"`
		RefusedPlain  string `json:"refused_plain"`
		RefusedBlob   string `json:"refused_blob"`

		// ApprovalCode is what ApprovalCode gives for PublicKey.
		ApprovalCode string `json:"approval_code"`
	} `json:"enroll"`
}

// VectorCase pins down one document: the bytes that get signed (as hex, so a
// port can check its byte assembly without touching Ed25519) and the resulting
// signature.
type VectorCase struct {
	Name        string          `json:"name"`
	Document    json.RawMessage `json:"document"`
	SignedBytes string          `json:"signed_bytes_hex"`
	Sig         string          `json:"sig"`
}

func buildVectors(t *testing.T) Vectors {
	t.Helper()
	root := ed25519.NewKeyFromSeed(rootSeed)
	backup := ed25519.NewKeyFromSeed(backupSeed)
	server := ed25519.NewKeyFromSeed(serverSeed)

	var v Vectors
	v.Note = "Контрольные векторы для spec/format.md. Реализация на Dart обязана " +
		"воспроизводить signed_bytes_hex и sig в точности. Сгенерировано " +
		"go test ./internal/proto -update."
	v.Keys.RootSeed = Encode(rootSeed)
	v.Keys.RootPub = Encode(PublicOf(root))
	v.Keys.BackupSeed = Encode(backupSeed)
	v.Keys.BackupPub = Encode(PublicOf(backup))
	v.Keys.ServerSeed = Encode(serverSeed)
	v.Keys.ServerPub = Encode(PublicOf(server))
	v.Keys.ContentKey = Encode(testContent)

	add := func(name string, doc any, signed []byte, sig string) {
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		v.Cases = append(v.Cases, VectorCase{
			Name:        name,
			Document:    raw,
			SignedBytes: hex.EncodeToString(signed),
			Sig:         sig,
		})
	}

	// cert, signed by root
	cert := Cert{
		Epoch:     3,
		NotBefore: testIat,
		NotAfter:  testExp,
		ServerPub: Encode(PublicOf(server)),
	}
	if err := cert.Sign(IssuerRoot, root); err != nil {
		t.Fatalf("sign cert: %v", err)
	}
	certBytes, err := cert.signedBytes()
	if err != nil {
		t.Fatalf("cert signed bytes: %v", err)
	}
	add("cert", cert, certBytes, cert.Sig)

	// config document, signed by the server key
	cfg := &Doc{
		Kind:      KindConfig,
		Seq:       7,
		IssuedAt:  testIat,
		ExpiresAt: testExp,
		Cert:      cert,
		Body:      testBody,
	}
	if err := cfg.Sign(server); err != nil {
		t.Fatalf("sign config: %v", err)
	}
	cfgBytes, err := cfg.signedBytes()
	if err != nil {
		t.Fatalf("config signed bytes: %v", err)
	}
	add("config", cfg, cfgBytes, cfg.Sig)

	// revocation, signed by backup
	rev := &Revocation{MinEpoch: 4, IssuedAt: testIat}
	if err := rev.Sign(IssuerBackup, backup); err != nil {
		t.Fatalf("sign revocation: %v", err)
	}
	add("revocation", rev, rev.signedBytes(), rev.Sig)

	// command, signed by root
	cmd := &Command{
		ID:       Encode(repeat(0x06)[:CommandIDSize]),
		IssuedAt: testIat,
		Action:   ActionMoveRegion,
		Arg:      "waw",
	}
	if err := cmd.Sign(IssuerRoot, root); err != nil {
		t.Fatalf("sign command: %v", err)
	}
	cmdBytes, err := cmd.signedBytes()
	if err != nil {
		t.Fatalf("command signed bytes: %v", err)
	}
	add("command", cmd, cmdBytes, cmd.Sig)

	// release announcement, signed by root
	rel := &Release{
		Version:  "1.2.0",
		Build:    12,
		MinBuild: 3,
		IssuedAt: testIat,
		Notes:    "Новая версия: быстрее подключается.",
		Assets: []ReleaseAsset{
			{OS: OSWindows, URL: "https://example.org/releases/Rebirth-Setup-1.2.0.exe", SHA256: strings.Repeat("ab", 32), Size: 28311552},
			{OS: OSAndroid, URL: "https://example.org/releases/Rebirth-1.2.0.apk", SHA256: strings.Repeat("cd", 32), Size: 19004416},
		},
	}
	if err := rel.Sign(IssuerRoot, root); err != nil {
		t.Fatalf("sign release: %v", err)
	}
	add("release", rel, rel.signedBytes(), rel.Sig)

	// envelope with a fixed nonce
	nonce := repeat(0x05)[:nonceSize]
	plain := []byte(`{"hello":"мир"}`)
	blob, err := sealWithNonce(testContent, nonce, plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	v.Envelope.Nonce = Encode(nonce)
	v.Envelope.Plaintext = Encode(plain)
	v.Envelope.Blob = blob

	// enrollment: a request, the reply granting an address, and a refusal
	e := &v.Enroll
	e.Now = testIat
	e.DeviceID = Encode(repeat(0x07)[:DeviceIDSize])
	e.PublicKey = Encode(repeat(0x08)[:TunnelKeySize])
	e.RequestNonce = Encode(repeat(0x09)[:EnrollNonceSize])
	e.Name = "windows"
	e.Address = "10.8.0.7/32"
	e.EnvelopeNonce = Encode(nonce)
	e.ApprovalCode = ApprovalCode(repeat(0x08)[:TunnelKeySize])

	req := EnrollRequest{
		V: FormatVersion, Kind: KindEnroll,
		DeviceID: e.DeviceID, PublicKey: e.PublicKey, Nonce: e.RequestNonce,
		IssuedAt: testIat, Name: e.Name,
	}
	reqPlain, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal enroll request: %v", err)
	}
	reply := NewEnrolled(&req, e.Address, time.Unix(testIat, 0))
	replyPlain, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("marshal enroll reply: %v", err)
	}
	refused := NewEnrollRefused(e.RequestNonce, EnrollFull, time.Unix(testIat, 0))
	refusedPlain, err := json.Marshal(refused)
	if err != nil {
		t.Fatalf("marshal enroll refusal: %v", err)
	}
	e.RequestPlain, e.ReplyPlain, e.RefusedPlain = string(reqPlain), string(replyPlain), string(refusedPlain)
	for _, s := range []struct {
		plain []byte
		blob  *string
	}{{reqPlain, &e.RequestBlob}, {replyPlain, &e.ReplyBlob}, {refusedPlain, &e.RefusedBlob}} {
		if *s.blob, err = sealWithNonce(testContent, nonce, s.plain); err != nil {
			t.Fatalf("seal enrollment: %v", err)
		}
	}

	return v
}

func TestVectorsMatchSpec(t *testing.T) {
	got := buildVectors(t)

	if *update {
		raw, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		raw = append(raw, '\n')
		if err := os.MkdirAll(filepath.Dir(vectorsPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(vectorsPath, raw, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Logf("wrote %s", vectorsPath)
		return
	}

	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("read vectors (run with -update to create them): %v", err)
	}
	var want Vectors
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}

	if len(got.Cases) != len(want.Cases) {
		t.Fatalf("%d cases, vectors have %d", len(got.Cases), len(want.Cases))
	}
	for i := range got.Cases {
		g, w := got.Cases[i], want.Cases[i]
		if g.Name != w.Name {
			t.Errorf("case %d: name %q, want %q", i, g.Name, w.Name)
			continue
		}
		if g.SignedBytes != w.SignedBytes {
			t.Errorf("%s: signed bytes changed\n got %s\nwant %s", g.Name, g.SignedBytes, w.SignedBytes)
		}
		if g.Sig != w.Sig {
			t.Errorf("%s: signature changed\n got %s\nwant %s", g.Name, g.Sig, w.Sig)
		}
	}
	if got.Envelope.Blob != want.Envelope.Blob {
		t.Errorf("envelope blob changed\n got %s\nwant %s", got.Envelope.Blob, want.Envelope.Blob)
	}
	if got.Enroll != want.Enroll {
		t.Errorf("enrollment vectors changed\n got %+v\nwant %+v", got.Enroll, want.Enroll)
	}
	if got.Keys.ServerPub != want.Keys.ServerPub {
		t.Errorf("server pub changed: %s vs %s", got.Keys.ServerPub, want.Keys.ServerPub)
	}
}

// The vectors must also verify through the public API, so a port can use them
// as end-to-end fixtures and not just as byte comparisons.
func TestVectorEnvelopeOpens(t *testing.T) {
	v := buildVectors(t)
	plain, err := Open(testContent, v.Envelope.Blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want, err := Decode(v.Envelope.Plaintext, 0)
	if err != nil {
		t.Fatalf("decode plaintext: %v", err)
	}
	if string(plain) != string(want) {
		t.Errorf("plaintext = %q, want %q", plain, want)
	}
}
