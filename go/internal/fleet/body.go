// Package fleet describes what one generation of servers publishes about
// itself. This is the plaintext that goes inside the signed, encrypted
// document in the gist.
package fleet

import (
	"encoding/json"
	"fmt"

	"awgrotator/internal/awg"
)

// BodyVersion is the layout version of the published configuration. Version 2
// replaced the one shared client key with the enrollment peer: every device now
// registers its own key. Version 1 can still be read, so the tools keep working
// against a server that has not been replaced yet.
const BodyVersion = 2

// OldestBodyVersion is the oldest layout this build still reads.
const OldestBodyVersion = 1

// Tunnel addressing. The server takes .1, devices get addresses from .2 to .249
// when they register, and the top of the range is reserved for peers that are
// not devices: .250 for the health check, .251 for enrollment.
const (
	Subnet        = "10.8.0.0/24"
	ServerAddress = "10.8.0.1/24"
	EnrollPeer    = "10.8.0.251/32"
)

// DNS is what clients resolve through while the tunnel is up. Phase 2
// replaces this with the server's own resolver.
var DNS = []string{"1.1.1.1", "9.9.9.9"}

// SharedClient is the single client configuration that version 1 published for
// everyone. It is only read, from a server that predates registration; nothing
// writes it any more.
type SharedClient struct {
	PrivateKey awg.Key `json:"private_key"`
	Address    string  `json:"address"`

	// Conf is the rendered wg-quick file, ready to import.
	Conf string `json:"conf"`
}

// EnrollInfo is what a device needs to register itself. The key is the same for
// every device and is published to everyone who has the app; the server lets
// it do nothing but reach the registration service (see spec/format.md 4.5).
type EnrollInfo struct {
	// PrivateKey and Address are the enrollment peer's identity inside the
	// tunnel.
	PrivateKey awg.Key `json:"private_key"`
	Address    string  `json:"address"`

	// Server is where registration is answered, as an address inside the
	// tunnel.
	Server string `json:"server"`
}

// ConfigBody is the plaintext of the gist's configuration document.
type ConfigBody struct {
	Version   int    `json:"version"`
	Region    string `json:"region"`
	Endpoint  string `json:"endpoint"`
	CreatedAt int64  `json:"created_at"`

	ServerPublicKey awg.Key    `json:"server_public_key"`
	Params          awg.Params `json:"params"`

	Subnet     string   `json:"subnet"`
	DNS        []string `json:"dns"`
	MTU        int      `json:"mtu"`
	AllowedIPs []string `json:"allowed_ips"`

	// Enroll is how a device registers itself. Present from version 2.
	Enroll *EnrollInfo `json:"enroll,omitempty"`

	// Shared is the version 1 shared client. Absent from version 2.
	Shared *SharedClient `json:"shared,omitempty"`
}

// Validate checks a body read from the gist before anything acts on it.
func (b *ConfigBody) Validate() error {
	if b.Version < OldestBodyVersion || b.Version > BodyVersion {
		return fmt.Errorf("configuration body version %d, this build understands %d to %d",
			b.Version, OldestBodyVersion, BodyVersion)
	}
	if b.Version >= 2 {
		if b.Enroll == nil || b.Enroll.PrivateKey.IsZero() || b.Enroll.Address == "" || b.Enroll.Server == "" {
			return fmt.Errorf("no way for a device to register")
		}
	}
	if b.Endpoint == "" {
		return fmt.Errorf("no endpoint")
	}
	if b.ServerPublicKey.IsZero() {
		return fmt.Errorf("no server public key")
	}
	if err := b.Params.Validate(); err != nil {
		return fmt.Errorf("obfuscation parameters: %w", err)
	}
	if len(b.AllowedIPs) == 0 {
		return fmt.Errorf("no allowed addresses")
	}
	return nil
}

// ClientConf renders the configuration file for one device: its own key and the
// address the server gave it, with everything else taken from the body. Both our
// apps and the stock AmneziaWG clients read this form. The Dart port of this
// function is held to spec/conf-vectors.json.
func (b *ConfigBody) ClientConf(private awg.Key, address string) string {
	return awg.ClientConfig{
		PrivateKey:      private,
		Address:         address,
		DNS:             b.DNS,
		MTU:             b.MTU,
		Params:          b.Params,
		ServerPublicKey: b.ServerPublicKey,
		Endpoint:        b.Endpoint,
		AllowedIPs:      b.AllowedIPs,
	}.Render()
}

// Marshal renders the body for signing. The bytes are signed verbatim, so
// whatever this produces is what gets verified.
func (b *ConfigBody) Marshal() ([]byte, error) {
	return json.Marshal(b)
}

// ParseBody reads and validates a body.
func ParseBody(raw []byte) (*ConfigBody, error) {
	var b ConfigBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("configuration body is not valid JSON: %w", err)
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &b, nil
}
