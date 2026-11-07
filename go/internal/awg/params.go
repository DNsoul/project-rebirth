package awg

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

const (
	// HeaderNonceSize mirrors device.HeaderCipherNonceSize in amneziawg-go.
	// Header protection uses the first bytes of every junk prefix as its
	// nonce, so with a header protection key all four S values must be at
	// least this large or the device refuses the configuration.
	HeaderNonceSize = 12

	// MTU is what Amnezia recommends for 3.1. The 2.x value of 1376 is too
	// large once the extra padding is in play.
	MTU = 1280

	// Keepalive keeps the mapping open on NAT in the middle.
	Keepalive = 25
)

// Params are the AmneziaWG 3.1 obfuscation parameters for one generation of
// the fleet.
//
// S1-S4, HeaderProtectionKey and RandomTrailers must be identical on both
// ends of the tunnel, so they are generated once per server and copied into
// every client configuration. Jc, Jmin, Jmax and the I-packets are allowed to
// differ per client.
//
// H1-H4 are deliberately absent. In 2.x they carried custom packet
// identifiers, but 3.1 protects the headers with a cipher instead, and the
// documentation says to leave H1-H4 at their defaults whenever a header
// protection key is set.
type Params struct {
	// Junk packets sent once before the handshake.
	Jc   int `json:"jc"`
	Jmin int `json:"jmin"`
	Jmax int `json:"jmax"`

	// Random prefix lengths for the init, response, cookie and data packets.
	S1 int `json:"s1"`
	S2 int `json:"s2"`
	S3 int `json:"s3"`
	S4 int `json:"s4"`

	// HeaderProtectionKey encrypts the packet headers. This is the 3.1
	// feature that replaced the magic headers of 2.x.
	HeaderProtectionKey Key `json:"header_protection_key"`

	// RandomTrailers appends a random number of bytes to handshake packets.
	RandomTrailers bool `json:"random_trailers"`

	// I1 is an obfuscation packet sent ahead of the handshake, written in
	// Amnezia's notation: <r N> is N random bytes.
	I1 string `json:"i1"`
}

// intRange is an inclusive range the generator draws from.
type intRange struct{ min, max int }

// The ranges new parameters are drawn from.
//
// S4 prefixes every data packet, so it stays tight: it is permanent overhead
// on all traffic, and on Windows the whole datagram has to fit in 2016 bytes.
// The other three only affect handshake packets and can be more generous.
var (
	jcRange   = intRange{3, 8}
	jminRange = intRange{40, 120}
	jmaxRange = intRange{400, 1000}
	sRange    = intRange{HeaderNonceSize, 48}
	s4Range   = intRange{HeaderNonceSize, 24}
	i1Range   = intRange{40, 120}

	// portRange avoids 51820, the WireGuard default, which is itself a
	// signature worth not carrying.
	portRange = intRange{20000, 60000}
)

// GenerateParams draws a fresh set of obfuscation parameters. Every server
// generation gets its own, so two generations never look alike on the wire.
func GenerateParams() (Params, error) {
	p := Params{RandomTrailers: true}

	for _, f := range []struct {
		dst *int
		r   intRange
	}{
		{&p.Jc, jcRange},
		{&p.Jmin, jminRange},
		{&p.Jmax, jmaxRange},
		{&p.S1, sRange},
		{&p.S2, sRange},
		{&p.S3, sRange},
		{&p.S4, s4Range},
	} {
		v, err := randIn(f.r)
		if err != nil {
			return Params{}, err
		}
		*f.dst = v
	}

	key, err := GenerateKey()
	if err != nil {
		return Params{}, err
	}
	p.HeaderProtectionKey = key

	n, err := randIn(i1Range)
	if err != nil {
		return Params{}, err
	}
	p.I1 = fmt.Sprintf("<r %d>", n)

	if err := p.Validate(); err != nil {
		// A generator that can produce an invalid set is a bug, not a
		// runtime condition.
		return Params{}, fmt.Errorf("generated parameters are invalid: %w", err)
	}
	return p, nil
}

// RandomPort picks the UDP port a server listens on.
func RandomPort() (int, error) { return randIn(portRange) }

// Validate checks the constraints amneziawg-go enforces, so a bad set is
// caught here rather than on a server that then fails to come up.
func (p Params) Validate() error {
	if p.Jc < 0 || p.Jmin < 0 || p.Jmax < 0 {
		return fmt.Errorf("junk parameters must not be negative")
	}
	if p.Jmin > p.Jmax {
		return fmt.Errorf("Jmin %d is greater than Jmax %d", p.Jmin, p.Jmax)
	}
	for _, s := range []struct {
		name  string
		value int
	}{{"S1", p.S1}, {"S2", p.S2}, {"S3", p.S3}, {"S4", p.S4}} {
		if s.value < 0 {
			return fmt.Errorf("%s must not be negative", s.name)
		}
		if !p.HeaderProtectionKey.IsZero() && s.value < HeaderNonceSize {
			return fmt.Errorf("%s is %d, but header protection needs at least %d",
				s.name, s.value, HeaderNonceSize)
		}
	}
	return nil
}

func randIn(r intRange) (int, error) {
	if r.max < r.min {
		return 0, fmt.Errorf("empty range %d-%d", r.min, r.max)
	}
	span := big.NewInt(int64(r.max - r.min + 1))
	n, err := rand.Int(rand.Reader, span)
	if err != nil {
		return 0, fmt.Errorf("draw random number: %w", err)
	}
	return r.min + int(n.Int64()), nil
}
