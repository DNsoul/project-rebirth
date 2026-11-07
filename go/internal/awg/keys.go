// Package awg generates AmneziaWG 3.1 keys and obfuscation parameters and
// renders them in the two forms that matter: the UAPI text the node pushes
// into amneziawg-go, and the wg-quick style file the clients read.
//
// The two forms encode keys differently — hex on the socket, base64 in the
// file — which is the kind of detail that is easy to get wrong once and then
// debug for an hour.
package awg

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/curve25519"
)

// KeySize is the length of a Curve25519 key, the same as WireGuard uses.
const KeySize = 32

// Key is a Curve25519 private or public key, or the header protection key.
type Key [KeySize]byte

// Keypair is one end of a tunnel.
type Keypair struct {
	Private Key
	Public  Key
}

// GenerateKeypair makes a new key pair, clamping the private half the way
// Curve25519 requires.
func GenerateKeypair() (Keypair, error) {
	var kp Keypair
	if _, err := rand.Read(kp.Private[:]); err != nil {
		return Keypair{}, fmt.Errorf("generate private key: %w", err)
	}
	kp.Private.clamp()

	pub, err := curve25519.X25519(kp.Private[:], curve25519.Basepoint)
	if err != nil {
		return Keypair{}, fmt.Errorf("derive public key: %w", err)
	}
	copy(kp.Public[:], pub)
	return kp, nil
}

// GenerateKey returns KeySize random bytes, used for the header protection
// key. Unlike a Curve25519 private key it is not clamped: it is a symmetric
// cipher key, not a scalar.
func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("generate key: %w", err)
	}
	return k, nil
}

func (k *Key) clamp() {
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
}

// IsZero reports whether the key is unset.
func (k Key) IsZero() bool {
	var zero Key
	return subtle.ConstantTimeCompare(k[:], zero[:]) == 1
}

// Base64 is how a key appears in a .conf file.
func (k Key) Base64() string { return base64.StdEncoding.EncodeToString(k[:]) }

// Hex is how a key appears on the UAPI socket.
func (k Key) Hex() string { return hex.EncodeToString(k[:]) }

// ParseKeyBase64 reads a key from its .conf form.
func ParseKeyBase64(s string) (Key, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return Key{}, fmt.Errorf("key is not valid base64: %w", err)
	}
	if len(raw) != KeySize {
		return Key{}, fmt.Errorf("key is %d bytes, want %d", len(raw), KeySize)
	}
	var k Key
	copy(k[:], raw)
	return k, nil
}

// PublicFrom derives the public key for a private key, so a stored private
// key can be checked against the public one a peer was given.
func PublicFrom(private Key) (Key, error) {
	pub, err := curve25519.X25519(private[:], curve25519.Basepoint)
	if err != nil {
		return Key{}, fmt.Errorf("derive public key: %w", err)
	}
	var k Key
	copy(k[:], pub)
	return k, nil
}
