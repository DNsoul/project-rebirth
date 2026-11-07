// Package soak runs long-lived tunnels of different kinds side by side from one
// machine and records, minute by minute, which of them keep working.
//
// It exists because the only way to learn how long a kind of tunnel survives
// from a given network is to use it from that network, the way a person would,
// for as long as it lives. A server that nobody connects to proves nothing, and
// neither does a tunnel that stays up: a session that is already established
// can keep working long after a fresh connection to the same server would be
// refused. So every tunnel here carries realistic traffic all the time, is
// torn down and brought up again on a schedule, and its server is also tried
// directly, outside the tunnel, which is where a block shows first.
package soak

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"time"

	"awgrotator/internal/awg"
)

// Duration is a time.Duration that reads and writes "4h" or "30s" in JSON.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf(`a duration such as "4h" is expected: %w`, err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Kinds of tunnel an arm can be.
const (
	// KindAWG is an AmneziaWG tunnel run in this process, with no device and no
	// privileges.
	KindAWG = "awg"

	// KindSOCKS is a tunnel run elsewhere (an OpenVPN client on this machine's
	// Linux subsystem) that is reached through a local SOCKS5 proxy.
	KindSOCKS = "socks"
)

// AWGTarget is everything needed to bring an AmneziaWG client up.
type AWGTarget struct {
	Endpoint      string     `json:"endpoint"`
	ServerPublic  awg.Key    `json:"server_public"`
	ClientPrivate awg.Key    `json:"client_private"`
	ClientAddr    string     `json:"client_addr"`
	Params        awg.Params `json:"params"`
}

// Arm is one tunnel under test.
type Arm struct {
	// Name labels the arm in every log line and names its log file.
	Name string `json:"name"`
	Kind string `json:"kind"`

	// ServerIP is the address of the server, tried directly (outside the
	// tunnel) for the warning a block gives before the tunnel itself breaks.
	ServerIP string `json:"server_ip"`

	// DirectTCP lists the TCP ports of the server to try directly.
	DirectTCP []int `json:"direct_tcp"`

	AWG *AWGTarget `json:"awg,omitempty"`

	// SOCKS is the local proxy a KindSOCKS arm is reached through, and Restart
	// the command that takes its tunnel down and brings it up again.
	SOCKS   string   `json:"socks,omitempty"`
	Restart []string `json:"restart,omitempty"`
}

// Config is a whole run.
type Config struct {
	Arms []Arm `json:"arms"`

	// ReconnectEvery is how often each tunnel is torn down and brought up
	// again, and Stagger the delay between one arm's turn and the next, so that
	// the arms are not all down at the same moment.
	ReconnectEvery Duration `json:"reconnect_every"`
	Stagger        Duration `json:"stagger"`

	// ProbeEvery is how often each tunnel is tried from inside, and
	// DirectEvery how often each server is tried from outside.
	ProbeEvery  Duration `json:"probe_every"`
	DirectEvery Duration `json:"direct_every"`

	// DailyGB is the volume each arm aims for, and StreamKBps the speed of one
	// long download.
	DailyGB    float64 `json:"daily_gb"`
	StreamKBps int     `json:"stream_kbps"`

	LogDir string `json:"log_dir"`
}

var armName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// WithDefaults fills in what was left out.
func (c Config) WithDefaults() Config {
	if c.ReconnectEvery == 0 {
		c.ReconnectEvery = Duration(4 * time.Hour)
	}
	if c.Stagger == 0 {
		c.Stagger = Duration(20 * time.Minute)
	}
	if c.ProbeEvery == 0 {
		c.ProbeEvery = Duration(30 * time.Second)
	}
	if c.DirectEvery == 0 {
		c.DirectEvery = Duration(60 * time.Second)
	}
	if c.DailyGB == 0 {
		c.DailyGB = 4
	}
	if c.StreamKBps == 0 {
		c.StreamKBps = 1500
	}
	if c.LogDir == "" {
		c.LogDir = "soak-logs"
	}
	return c
}

// Validate refuses a configuration that cannot run.
func (c Config) Validate() error {
	if len(c.Arms) == 0 {
		return fmt.Errorf("no arms to run")
	}
	seen := map[string]bool{}
	for _, a := range c.Arms {
		if !armName.MatchString(a.Name) {
			return fmt.Errorf("arm name %q: lowercase letters, digits and dashes only", a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("arm %q is listed twice", a.Name)
		}
		seen[a.Name] = true
		if _, err := netip.ParseAddr(a.ServerIP); err != nil {
			return fmt.Errorf("arm %s: server_ip: %w", a.Name, err)
		}
		switch a.Kind {
		case KindAWG:
			if a.AWG == nil {
				return fmt.Errorf("arm %s: an awg arm needs an awg section", a.Name)
			}
			if _, err := netip.ParseAddrPort(a.AWG.Endpoint); err != nil {
				return fmt.Errorf("arm %s: endpoint: %w", a.Name, err)
			}
			if _, err := netip.ParseAddr(a.AWG.ClientAddr); err != nil {
				return fmt.Errorf("arm %s: client_addr: %w", a.Name, err)
			}
			if err := a.AWG.Params.Validate(); err != nil {
				return fmt.Errorf("arm %s: %w", a.Name, err)
			}
		case KindSOCKS:
			if a.SOCKS == "" {
				return fmt.Errorf("arm %s: a socks arm needs the proxy address", a.Name)
			}
			if len(a.Restart) == 0 {
				return fmt.Errorf("arm %s: a socks arm needs the command that restarts its tunnel", a.Name)
			}
		default:
			return fmt.Errorf("arm %s: kind %q is not awg or socks", a.Name, a.Kind)
		}
	}
	if c.ReconnectEvery.D() < time.Minute && c.ReconnectEvery != 0 {
		return fmt.Errorf("reconnecting more often than once a minute is not a soak test")
	}
	return nil
}

// Load reads a configuration file and applies the defaults.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	c = c.WithDefaults()
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
