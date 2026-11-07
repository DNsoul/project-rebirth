package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// ContentKeySize is the length of the symmetric key that encrypts everything
// published to the gist. It is baked into every app build: holding the app is
// what grants read access. It is deliberately NOT what prevents a substituted
// server — that is the job of the signatures.
const ContentKeySize = 32

// CommandIDSize is the length of the random identifier carried by a command,
// used by the server to reject a replayed command.
const CommandIDSize = 16

// b64 encodes every binary value carried inside JSON.
var b64 = base64.RawURLEncoding

// Issuers allowed to sign certificates, revocations and commands. The root key
// lives in the phone's Keystore; the backup key lives offline and exists so a
// lost phone can be revoked.
const (
	IssuerRoot   = "root"
	IssuerBackup = "backup"
)

// GenerateSigningKey returns a fresh Ed25519 private key.
func GenerateSigningKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	return priv, nil
}

// GenerateContentKey returns a fresh symmetric key for the envelope.
func GenerateContentKey() ([]byte, error) {
	k := make([]byte, ContentKeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("generate content key: %w", err)
	}
	return k, nil
}

// NewCommandID returns a fresh random command identifier.
func NewCommandID() (string, error) {
	id := make([]byte, CommandIDSize)
	if _, err := rand.Read(id); err != nil {
		return "", fmt.Errorf("generate command id: %w", err)
	}
	return Encode(id), nil
}

// Encode renders binary data the way it appears inside every document.
func Encode(b []byte) string { return b64.EncodeToString(b) }

// Decode parses such a value. When want is non-zero the length is enforced.
func Decode(s string, want int) ([]byte, error) {
	b, err := b64.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not valid base64url: %w", err)
	}
	if want > 0 && len(b) != want {
		return nil, fmt.Errorf("length %d, want %d", len(b), want)
	}
	return b, nil
}

// PublicOf returns the public half of a signing key.
func PublicOf(priv ed25519.PrivateKey) ed25519.PublicKey {
	return priv.Public().(ed25519.PublicKey)
}
