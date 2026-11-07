package proto

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Enrollment is how a device gets its own place on a server without anyone
// registering it: the app proves it holds the content key, says who it is, and
// the server adds it. See spec/format.md section 4.5.
//
// Both messages are ordinary envelopes (section 2), so they carry no
// recognisable marker, and both travel inside the tunnel, so nothing about them
// is visible from outside.

// Enrollment message kinds.
const (
	KindEnroll   = "enroll"
	KindEnrolled = "enrolled"
)

// Sizes of the binary fields.
const (
	DeviceIDSize    = 16 // a hash of something unique to the device
	EnrollNonceSize = 16 // random, echoed in the reply so a stale one is recognised
	TunnelKeySize   = 32 // a Curve25519 public key, as WireGuard uses
)

// MaxDeviceName is how long the free-text label may be. It is for the
// operator's eyes ("windows", "phone"), nothing depends on it.
const MaxDeviceName = 32

// Reasons an enrollment is refused. They are part of the format: an app can
// react to them, for example by waiting and trying again after ErrBusy.
const (
	EnrollBadRequest = "bad_request"
	EnrollBusy       = "busy"       // too many requests just now; try again shortly
	EnrollFull       = "full"       // the server holds as many devices as it will
	EnrollKeyInUse   = "key_in_use" // that key already belongs to another device
)

// EnrollRequest is what a device sends.
type EnrollRequest struct {
	V    int    `json:"v"`
	Kind string `json:"kind"`

	// DeviceID identifies the device across reinstalls and servers. Enrolling
	// again with the same ID replaces the key and keeps the address.
	DeviceID string `json:"id"`

	// PublicKey is the device's own tunnel key. The private half never leaves
	// the device.
	PublicKey string `json:"pub"`

	Nonce    string `json:"nonce"`
	IssuedAt int64  `json:"iat"`
	Name     string `json:"name,omitempty"`
}

// EnrollResponse is what the server answers.
type EnrollResponse struct {
	V    int    `json:"v"`
	Kind string `json:"kind"`

	// Nonce repeats the request's, binding the answer to the question.
	Nonce string `json:"nonce"`

	// Address is the device's address inside the tunnel, with prefix, when the
	// enrollment succeeded.
	Address string `json:"address,omitempty"`

	// Error is one of the Enroll* reasons when it did not.
	Error string `json:"error,omitempty"`

	IssuedAt int64 `json:"iat"`
}

// NewEnrollRequest fills in a request with a fresh nonce.
func NewEnrollRequest(deviceID, tunnelKey []byte, name string, now time.Time) (*EnrollRequest, error) {
	nonce, err := randomBytes(EnrollNonceSize)
	if err != nil {
		return nil, err
	}
	r := &EnrollRequest{
		V:         FormatVersion,
		Kind:      KindEnroll,
		DeviceID:  Encode(deviceID),
		PublicKey: Encode(tunnelKey),
		Nonce:     Encode(nonce),
		IssuedAt:  now.Unix(),
		Name:      name,
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *EnrollRequest) check() error {
	if r.V != FormatVersion {
		return fmt.Errorf("enrollment format version %d, this build understands %d", r.V, FormatVersion)
	}
	if r.Kind != KindEnroll {
		return fmt.Errorf("expected an %q message, got %q", KindEnroll, r.Kind)
	}
	if _, err := Decode(r.DeviceID, DeviceIDSize); err != nil {
		return fmt.Errorf("device id: %w", err)
	}
	pub, err := Decode(r.PublicKey, TunnelKeySize)
	if err != nil {
		return fmt.Errorf("public key: %w", err)
	}
	if bytes.Equal(pub, make([]byte, TunnelKeySize)) {
		return errors.New("public key is all zeros")
	}
	if _, err := Decode(r.Nonce, EnrollNonceSize); err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	if utf8.RuneCountInString(r.Name) > MaxDeviceName {
		return fmt.Errorf("device name is longer than %d characters", MaxDeviceName)
	}
	return nil
}

// Seal encrypts the request for sending.
func (r *EnrollRequest) Seal(contentKey []byte) (string, error) {
	if err := r.check(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return Seal(contentKey, raw)
}

// OpenEnrollRequest decrypts and checks a request. now is the server's clock;
// a request outside the usual tolerance of it is refused, which is what keeps a
// recorded request from being replayed much later.
func OpenEnrollRequest(contentKey []byte, blob string, now time.Time) (*EnrollRequest, error) {
	plain, err := Open(contentKey, blob)
	if err != nil {
		return nil, err
	}
	var r EnrollRequest
	if err := json.Unmarshal(plain, &r); err != nil {
		return nil, fmt.Errorf("enrollment is not valid JSON: %w", err)
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	if d := now.Sub(time.Unix(r.IssuedAt, 0)); d > ClockSkew || d < -ClockSkew {
		return nil, fmt.Errorf("enrollment is dated %s from now, more than the %s tolerated", d.Round(time.Second), ClockSkew)
	}
	r.Name = cleanName(r.Name)
	return &r, nil
}

// cleanName keeps the label printable: it ends up in logs and in the operator's
// listing, and a control character there would make a mess of either.
func cleanName(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// NewEnrolled answers a request with an address.
func NewEnrolled(req *EnrollRequest, address string, now time.Time) *EnrollResponse {
	return &EnrollResponse{V: FormatVersion, Kind: KindEnrolled, Nonce: req.Nonce, Address: address, IssuedAt: now.Unix()}
}

// NewEnrollRefused answers a request with a reason. nonce may be empty when the
// request could not even be read.
func NewEnrollRefused(nonce, reason string, now time.Time) *EnrollResponse {
	return &EnrollResponse{V: FormatVersion, Kind: KindEnrolled, Nonce: nonce, Error: reason, IssuedAt: now.Unix()}
}

// Seal encrypts the response for sending.
func (r *EnrollResponse) Seal(contentKey []byte) (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return Seal(contentKey, raw)
}

// OpenEnrollResponse decrypts a response and checks that it answers the request
// with the given nonce.
func OpenEnrollResponse(contentKey []byte, blob, wantNonce string) (*EnrollResponse, error) {
	plain, err := Open(contentKey, blob)
	if err != nil {
		return nil, err
	}
	var r EnrollResponse
	if err := json.Unmarshal(plain, &r); err != nil {
		return nil, fmt.Errorf("reply is not valid JSON: %w", err)
	}
	if r.V != FormatVersion || r.Kind != KindEnrolled {
		return nil, fmt.Errorf("not an enrollment reply (v%d, %q)", r.V, r.Kind)
	}
	if r.Nonce != wantNonce {
		return nil, errors.New("the reply answers a different request")
	}
	if r.Error == "" && r.Address == "" {
		return nil, errors.New("the reply neither grants an address nor says why not")
	}
	return &r, nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("random bytes: %w", err)
	}
	return b, nil
}
