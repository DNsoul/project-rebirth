package proto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const nonceSize = 12

// Seal encrypts plaintext with the content key and returns the text stored in
// the gist: base64url(nonce || ciphertext || tag).
//
// There is no magic header on purpose, so the stored text carries no
// recognisable marker of what it is.
func Seal(contentKey, plaintext []byte) (string, error) {
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return sealWithNonce(contentKey, nonce, plaintext)
}

// sealWithNonce is Seal with the nonce supplied by the caller. Only tests use
// it, to produce reproducible vectors; real callers must use Seal so that the
// nonce is always fresh.
func sealWithNonce(contentKey, nonce, plaintext []byte) (string, error) {
	aead, err := newAEAD(contentKey)
	if err != nil {
		return "", err
	}
	if len(nonce) != nonceSize {
		return "", fmt.Errorf("nonce must be %d bytes, got %d", nonceSize, len(nonce))
	}
	return Encode(aead.Seal(nonce, nonce, plaintext, nil)), nil
}

// Open reverses Seal.
func Open(contentKey []byte, blob string) ([]byte, error) {
	aead, err := newAEAD(contentKey)
	if err != nil {
		return nil, err
	}
	raw, err := Decode(blob, 0)
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	if len(raw) < nonceSize+aead.Overhead() {
		return nil, errors.New("blob is too short to be an envelope")
	}
	pt, err := aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		// Deliberately vague: a wrong key and a corrupted blob are the same
		// failure as far as the caller is concerned.
		return nil, errors.New("blob does not decrypt with this content key")
	}
	return pt, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != ContentKeySize {
		return nil, fmt.Errorf("content key must be %d bytes, got %d", ContentKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return aead, nil
}
