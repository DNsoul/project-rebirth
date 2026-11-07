package node

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"awgrotator/internal/splitdns"
)

// HandshakeFresh is how recently a peer must have completed a handshake for
// the node to consider it connected. WireGuard rekeys every two minutes, so
// three gives one missed attempt of slack.
const HandshakeFresh = 3 * time.Minute

// PeerStatus is what the node knows about one client.
type PeerStatus struct {
	PublicKey     string    `json:"public_key"`
	Endpoint      string    `json:"endpoint,omitempty"`
	LastHandshake time.Time `json:"last_handshake,omitempty"`
	RxBytes       int64     `json:"rx_bytes"`
	TxBytes       int64     `json:"tx_bytes"`
}

// Connected reports whether this peer handshook recently enough.
func (p PeerStatus) Connected(now time.Time) bool {
	return !p.LastHandshake.IsZero() && now.Sub(p.LastHandshake) < HandshakeFresh
}

// SplitStatus reports on the split-routing resolver. It carries counts only:
// the resolver sees every site a client visits, and the status is published,
// so nothing that names a site belongs here.
type SplitStatus struct {
	splitdns.Stats

	// RURanges is how many local address ranges were loaded. Zero means the
	// set is missing and every site is being tunnelled, which is safe but
	// not what was asked for, so deployment checks it.
	RURanges    int `json:"ru_ranges"`
	ForceTunnel int `json:"force_tunnel"`
	ForceDirect int `json:"force_direct"`
}

// Status is the node's report about itself. awgctl reads it over SSH, and
// from phase 3 the node publishes it to the gist as well.
type Status struct {
	Interface  string       `json:"interface"`
	ListenPort int          `json:"listen_port"`
	StartedAt  time.Time    `json:"started_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	Peers      []PeerStatus `json:"peers"`
	Split      *SplitStatus `json:"split,omitempty"`
}

// AnyConnected reports whether at least one client is currently up.
func (s Status) AnyConnected() bool {
	for _, p := range s.Peers {
		if p.Connected(s.UpdatedAt) {
			return true
		}
	}
	return false
}

// ParseStatus reads a status file.
func ParseStatus(raw []byte) (Status, error) {
	var s Status
	if err := json.Unmarshal(raw, &s); err != nil {
		return Status{}, fmt.Errorf("status is not valid JSON: %w", err)
	}
	return s, nil
}

// ParseDeviceStatus reads what amneziawg-go returns from IpcGet.
//
// Keys are hex on the socket, so public keys are converted to the base64 form
// that appears everywhere a person might compare them.
func ParseDeviceStatus(uapi string) (listenPort int, peers []PeerStatus, err error) {
	var cur *PeerStatus
	flush := func() {
		if cur != nil {
			peers = append(peers, *cur)
			cur = nil
		}
	}

	for _, line := range strings.Split(uapi, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		switch key {
		case "public_key":
			flush()
			raw, decodeErr := hex.DecodeString(value)
			if decodeErr != nil {
				return 0, nil, fmt.Errorf("peer public key is not hex: %w", decodeErr)
			}
			cur = &PeerStatus{PublicKey: base64.StdEncoding.EncodeToString(raw)}
			continue
		case "listen_port":
			if cur == nil {
				listenPort, _ = strconv.Atoi(value)
			}
			continue
		case "errno":
			continue
		}

		if cur == nil {
			// A device-level field we do not report on. Private keys land
			// here and are deliberately dropped.
			continue
		}

		switch key {
		case "endpoint":
			cur.Endpoint = value
		case "last_handshake_time_sec":
			sec, _ := strconv.ParseInt(value, 10, 64)
			if sec > 0 {
				cur.LastHandshake = time.Unix(sec, 0).UTC()
			}
		case "rx_bytes":
			cur.RxBytes, _ = strconv.ParseInt(value, 10, 64)
		case "tx_bytes":
			cur.TxBytes, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	flush()
	return listenPort, peers, nil
}
