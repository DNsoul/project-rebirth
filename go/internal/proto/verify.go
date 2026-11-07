package proto

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ClockSkew is how far a timestamp may sit outside the window before it is
// rejected, to tolerate imperfect clocks.
const ClockSkew = 5 * time.Minute

// CommandFreshness is how old a command may be. Together with the server
// remembering executed command IDs, this stops a captured command from being
// replayed later.
const CommandFreshness = 5 * time.Minute

// Trust is what a verifier is built with: the two keys it will accept
// signatures from, and the key that decrypts the envelope. In the apps all
// three are baked into the build.
type Trust struct {
	RootPub    string `json:"root_pub"`
	BackupPub  string `json:"backup_pub"`
	ContentKey string `json:"content_key"`
}

// State is what a verifier remembers between runs so that an old but
// correctly signed document cannot be replayed at it.
type State struct {
	MinEpoch uint32 `json:"min_epoch"`
	LastSeq  uint64 `json:"last_seq"`

	// LastBuild is the newest release announcement seen, so that an older one
	// cannot be replayed.
	LastBuild uint32 `json:"last_build,omitempty"`
}

func (t *Trust) issuerKey(name string) (ed25519.PublicKey, error) {
	var raw string
	switch name {
	case IssuerRoot:
		raw = t.RootPub
	case IssuerBackup:
		raw = t.BackupPub
	default:
		return nil, fmt.Errorf("unknown issuer %q", name)
	}
	if raw == "" {
		return nil, fmt.Errorf("no %s key configured", name)
	}
	b, err := Decode(raw, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("%s key: %w", name, err)
	}
	return ed25519.PublicKey(b), nil
}

func (t *Trust) contentKey() ([]byte, error) {
	return Decode(t.ContentKey, ContentKeySize)
}

// VerifyCert checks a certificate's signature, its epoch against the
// remembered minimum, and its validity window.
func (t *Trust) VerifyCert(c *Cert, now time.Time, st State) error {
	key, err := t.issuerKey(c.Issuer)
	if err != nil {
		return err
	}
	msg, err := c.signedBytes()
	if err != nil {
		return err
	}
	sig, err := Decode(c.Sig, ed25519.SignatureSize)
	if err != nil {
		return fmt.Errorf("cert signature: %w", err)
	}
	if !ed25519.Verify(key, msg, sig) {
		return fmt.Errorf("certificate is not signed by the %s key", c.Issuer)
	}
	if c.Epoch < st.MinEpoch {
		return fmt.Errorf("certificate epoch %d was revoked (minimum is %d)", c.Epoch, st.MinEpoch)
	}
	if err := checkWindow("certificate", c.NotBefore, c.NotAfter, now); err != nil {
		return err
	}
	return nil
}

// OpenDoc decrypts and fully verifies a document taken from the gist, in the
// order laid out in spec/format.md section 5. It returns the document and the
// state the caller should store. On any failure the caller's state must stay
// as it was.
func (t *Trust) OpenDoc(blob, wantKind string, now time.Time, st State) (*Doc, State, error) {
	ck, err := t.contentKey()
	if err != nil {
		return nil, st, fmt.Errorf("content key: %w", err)
	}
	plain, err := Open(ck, blob)
	if err != nil {
		return nil, st, err
	}

	var d Doc
	if err := json.Unmarshal(plain, &d); err != nil {
		return nil, st, fmt.Errorf("document is not valid JSON: %w", err)
	}
	if d.V != FormatVersion {
		return nil, st, fmt.Errorf("document format version %d, this build understands %d", d.V, FormatVersion)
	}
	if d.Kind != wantKind {
		return nil, st, fmt.Errorf("expected a %q document, got %q", wantKind, d.Kind)
	}
	if err := t.VerifyCert(&d.Cert, now, st); err != nil {
		return nil, st, err
	}

	pub, err := Decode(d.Cert.ServerPub, ed25519.PublicKeySize)
	if err != nil {
		return nil, st, fmt.Errorf("cert pub: %w", err)
	}
	msg, err := d.signedBytes()
	if err != nil {
		return nil, st, err
	}
	sig, err := Decode(d.Sig, ed25519.SignatureSize)
	if err != nil {
		return nil, st, fmt.Errorf("document signature: %w", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return nil, st, errors.New("document is not signed by the key its certificate names")
	}

	// A status report is a snapshot and may legitimately arrive out of order;
	// only a configuration must never go backwards.
	if d.Kind == KindConfig && d.Seq <= st.LastSeq {
		return nil, st, fmt.Errorf("document seq %d is not newer than the last seen %d", d.Seq, st.LastSeq)
	}
	if err := checkWindow("document", d.IssuedAt, d.ExpiresAt, now); err != nil {
		return nil, st, err
	}

	next := st
	if d.Kind == KindConfig {
		next.LastSeq = d.Seq
	}
	return &d, next, nil
}

// OpenRevocation decrypts and verifies a revocation record and returns the
// state it implies. A revocation may only raise the minimum epoch.
func (t *Trust) OpenRevocation(blob string, now time.Time, st State) (*Revocation, State, error) {
	ck, err := t.contentKey()
	if err != nil {
		return nil, st, fmt.Errorf("content key: %w", err)
	}
	plain, err := Open(ck, blob)
	if err != nil {
		return nil, st, err
	}

	var r Revocation
	if err := json.Unmarshal(plain, &r); err != nil {
		return nil, st, fmt.Errorf("revocation is not valid JSON: %w", err)
	}
	if r.V != FormatVersion {
		return nil, st, fmt.Errorf("revocation format version %d, this build understands %d", r.V, FormatVersion)
	}
	key, err := t.issuerKey(r.Issuer)
	if err != nil {
		return nil, st, err
	}
	sig, err := Decode(r.Sig, ed25519.SignatureSize)
	if err != nil {
		return nil, st, fmt.Errorf("revocation signature: %w", err)
	}
	if !ed25519.Verify(key, r.signedBytes(), sig) {
		return nil, st, fmt.Errorf("revocation is not signed by the %s key", r.Issuer)
	}
	if r.MinEpoch <= st.MinEpoch {
		return nil, st, fmt.Errorf("revocation minimum epoch %d does not raise the current %d", r.MinEpoch, st.MinEpoch)
	}

	next := st
	next.MinEpoch = r.MinEpoch
	return &r, next, nil
}

// OpenCommand decrypts and verifies a command from the super client. Replay
// protection is only half done here: the caller must also reject a command
// whose ID it has already executed.
func (t *Trust) OpenCommand(blob string, now time.Time) (*Command, error) {
	ck, err := t.contentKey()
	if err != nil {
		return nil, fmt.Errorf("content key: %w", err)
	}
	plain, err := Open(ck, blob)
	if err != nil {
		return nil, err
	}

	var c Command
	if err := json.Unmarshal(plain, &c); err != nil {
		return nil, fmt.Errorf("command is not valid JSON: %w", err)
	}
	if c.V != FormatVersion {
		return nil, fmt.Errorf("command format version %d, this build understands %d", c.V, FormatVersion)
	}
	key, err := t.issuerKey(c.Issuer)
	if err != nil {
		return nil, err
	}
	msg, err := c.signedBytes()
	if err != nil {
		return nil, err
	}
	sig, err := Decode(c.Sig, ed25519.SignatureSize)
	if err != nil {
		return nil, fmt.Errorf("command signature: %w", err)
	}
	if !ed25519.Verify(key, msg, sig) {
		return nil, fmt.Errorf("command is not signed by the %s key", c.Issuer)
	}

	age := now.Sub(time.Unix(c.IssuedAt, 0))
	if age > CommandFreshness || age < -ClockSkew {
		return nil, fmt.Errorf("command timestamp is outside the accepted window (off by %s)", age.Round(time.Second))
	}
	return &c, nil
}

func checkWindow(what string, notBefore, notAfter int64, now time.Time) error {
	if notAfter <= notBefore {
		return fmt.Errorf("%s validity window is empty", what)
	}
	if now.Add(ClockSkew).Before(time.Unix(notBefore, 0)) {
		return fmt.Errorf("%s is not valid yet", what)
	}
	if now.Add(-ClockSkew).After(time.Unix(notAfter, 0)) {
		return fmt.Errorf("%s has expired", what)
	}
	return nil
}

// SealDoc signs a document with the server key and wraps it for the gist.
func SealDoc(d *Doc, serverPriv ed25519.PrivateKey, contentKey []byte) (string, error) {
	if err := d.Sign(serverPriv); err != nil {
		return "", err
	}
	return sealJSON(d, contentKey)
}

// SealRevocation signs a revocation with the root or backup key and wraps it.
func SealRevocation(r *Revocation, issuer string, priv ed25519.PrivateKey, contentKey []byte) (string, error) {
	if err := r.Sign(issuer, priv); err != nil {
		return "", err
	}
	return sealJSON(r, contentKey)
}

// SealCommand signs a command with the root or backup key and wraps it.
func SealCommand(c *Command, issuer string, priv ed25519.PrivateKey, contentKey []byte) (string, error) {
	if err := c.Sign(issuer, priv); err != nil {
		return "", err
	}
	return sealJSON(c, contentKey)
}

func sealJSON(v any, contentKey []byte) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode document: %w", err)
	}
	return Seal(contentKey, plain)
}
