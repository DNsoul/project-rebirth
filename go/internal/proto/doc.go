package proto

import (
	"crypto/ed25519"
	"fmt"
)

// Signing domains. Each document kind has its own, so a signature made for one
// can never be accepted as a signature for another.
const (
	domainCert    = "awg-rotator/cert/v1"
	domainDoc     = "awg-rotator/doc/v1"
	domainRevoke  = "awg-rotator/revoke/v1"
	domainCommand = "awg-rotator/command/v1"
)

// Document kinds carried in Doc.Kind.
const (
	KindConfig = "config"
	KindStatus = "status"
)

// Command actions.
const (
	ActionMove       = "move"        // move to a new server now
	ActionMoveRegion = "move_region" // same, into the region named by Arg
	ActionStatus     = "status"      // publish a fresh status document
	ActionRevoke     = "revoke"      // raise the minimum epoch to Arg
)

// FormatVersion is written into every document and checked on the way in.
const FormatVersion = 1

// Cert states that a server signing key may be trusted. It is signed by the
// root key (in the phone's Keystore) or by the offline backup key.
type Cert struct {
	Epoch     uint32 `json:"epoch"`
	NotBefore int64  `json:"nbf"`
	NotAfter  int64  `json:"exp"`
	ServerPub string `json:"pub"`
	Issuer    string `json:"by"`
	Sig       string `json:"sig"`
}

func (c *Cert) signedBytes() ([]byte, error) {
	pub, err := Decode(c.ServerPub, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("cert pub: %w", err)
	}
	s := newSigBuf(domainCert)
	s.u32(c.Epoch)
	s.i64(c.NotBefore)
	s.i64(c.NotAfter)
	s.lp(pub)
	s.lps(c.Issuer)
	return s.bytes(), nil
}

// Sign fills in Issuer and Sig. priv must be the root or backup key.
func (c *Cert) Sign(issuer string, priv ed25519.PrivateKey) error {
	if issuer != IssuerRoot && issuer != IssuerBackup {
		return fmt.Errorf("unknown issuer %q", issuer)
	}
	c.Issuer = issuer
	msg, err := c.signedBytes()
	if err != nil {
		return err
	}
	c.Sig = Encode(ed25519.Sign(priv, msg))
	return nil
}

// Doc is a configuration or a status report. Both share one shape and differ
// only in Kind.
type Doc struct {
	V         int    `json:"v"`
	Kind      string `json:"kind"`
	Seq       uint64 `json:"seq"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Cert      Cert   `json:"cert"`

	// Body is opaque to this package and is signed exactly as stored. Keeping
	// it a string rather than a nested object means Go and Dart never have to
	// agree on how to re-serialise it.
	Body string `json:"body"`

	Sig string `json:"sig"`
}

func (d *Doc) signedBytes() ([]byte, error) {
	pub, err := Decode(d.Cert.ServerPub, ed25519.PublicKeySize)
	if err != nil {
		return nil, fmt.Errorf("cert pub: %w", err)
	}
	s := newSigBuf(domainDoc)
	s.lps(d.Kind)
	s.u64(d.Seq)
	s.i64(d.IssuedAt)
	s.i64(d.ExpiresAt)
	s.lp(pub)
	s.lps(d.Body)
	return s.bytes(), nil
}

// Sign fills in Sig. priv must be the server key named by the embedded cert.
func (d *Doc) Sign(priv ed25519.PrivateKey) error {
	if d.Kind != KindConfig && d.Kind != KindStatus {
		return fmt.Errorf("unknown document kind %q", d.Kind)
	}
	if got, want := Encode(PublicOf(priv)), d.Cert.ServerPub; got != want {
		return fmt.Errorf("signing key does not match the certificate it carries")
	}
	d.V = FormatVersion
	msg, err := d.signedBytes()
	if err != nil {
		return err
	}
	d.Sig = Encode(ed25519.Sign(priv, msg))
	return nil
}

// Revocation raises the minimum acceptable certificate epoch, which is how a
// lost phone or a seized server is cut off without rebuilding the apps.
type Revocation struct {
	V        int    `json:"v"`
	MinEpoch uint32 `json:"min_epoch"`
	IssuedAt int64  `json:"iat"`
	Issuer   string `json:"by"`
	Sig      string `json:"sig"`
}

func (r *Revocation) signedBytes() []byte {
	s := newSigBuf(domainRevoke)
	s.u32(r.MinEpoch)
	s.i64(r.IssuedAt)
	s.lps(r.Issuer)
	return s.bytes()
}

// Sign fills in Issuer and Sig. priv must be the root or backup key.
func (r *Revocation) Sign(issuer string, priv ed25519.PrivateKey) error {
	if issuer != IssuerRoot && issuer != IssuerBackup {
		return fmt.Errorf("unknown issuer %q", issuer)
	}
	r.V = FormatVersion
	r.Issuer = issuer
	r.Sig = Encode(ed25519.Sign(priv, r.signedBytes()))
	return nil
}

// Command is an instruction from the super client to the server. It is signed
// by the root or backup key directly; no certificate is involved.
type Command struct {
	V        int    `json:"v"`
	ID       string `json:"id"`
	IssuedAt int64  `json:"iat"`
	Action   string `json:"action"`
	Arg      string `json:"arg,omitempty"`
	Issuer   string `json:"by"`
	Sig      string `json:"sig"`
}

func (c *Command) signedBytes() ([]byte, error) {
	id, err := Decode(c.ID, CommandIDSize)
	if err != nil {
		return nil, fmt.Errorf("command id: %w", err)
	}
	s := newSigBuf(domainCommand)
	s.lp(id)
	s.i64(c.IssuedAt)
	s.lps(c.Action)
	s.lps(c.Arg)
	s.lps(c.Issuer)
	return s.bytes(), nil
}

// Sign fills in Issuer and Sig. priv must be the root or backup key.
func (c *Command) Sign(issuer string, priv ed25519.PrivateKey) error {
	if issuer != IssuerRoot && issuer != IssuerBackup {
		return fmt.Errorf("unknown issuer %q", issuer)
	}
	switch c.Action {
	case ActionMove, ActionMoveRegion, ActionStatus, ActionRevoke:
	default:
		return fmt.Errorf("unknown action %q", c.Action)
	}
	c.V = FormatVersion
	c.Issuer = issuer
	msg, err := c.signedBytes()
	if err != nil {
		return err
	}
	c.Sig = Encode(ed25519.Sign(priv, msg))
	return nil
}
