// Package awglib is the tunnel engine of the Android app. It reads the same
// client configuration file as every other client and runs it on a TUN device
// that the operating system hands over, using the same amneziawg-go the servers
// run, so that a client and a server cannot disagree about the protocol.
//
// It knows nothing about Android. The part that does is a few lines in
// cmd/awglib; everything here is tested on the development machine.
package awglib

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun"

	"awgrotator/internal/awg"
)

// Settings is a client configuration, read from a file.
type Settings struct {
	PrivateKey awg.Key
	Addresses  []netip.Prefix
	DNS        []netip.Addr
	MTU        int
	Params     awg.Params

	ServerPublic awg.Key
	Endpoint     netip.AddrPort
	AllowedIPs   []netip.Prefix
	Keepalive    int
}

// ParseConf reads a wg-quick style file as awg.ClientConfig.Render writes it.
//
// It is strict on purpose. A line it does not know is an error rather than
// something to skip: a misspelled HeaderProtectionKey that was skipped would
// give a tunnel that connects to nothing and says nothing about why.
func ParseConf(text string) (Settings, error) {
	var s Settings
	section := ""
	var peers int

	for n, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(strings.TrimPrefix(raw, awg.BOM))
		if i := strings.Index(line, "#"); i >= 0 && !strings.Contains(line, "<") {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		fail := func(format string, a ...any) (Settings, error) {
			return Settings{}, fmt.Errorf("line %d: %s", n+1, fmt.Sprintf(format, a...))
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line[1 : len(line)-1])
			switch section {
			case "interface":
			case "peer":
				peers++
				if peers > 1 {
					return fail("more than one [Peer]: a client talks to one server")
				}
			default:
				return fail("unknown section %q", line)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fail("expected key = value, got %q", line)
		}
		key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
		if section == "" {
			return fail("%q is outside any section", line)
		}

		var err error
		switch section + "." + key {
		case "interface.privatekey":
			s.PrivateKey, err = awg.ParseKeyBase64(value)
		case "interface.address":
			s.Addresses, err = prefixes(value, true)
		case "interface.dns":
			s.DNS, err = addrs(value)
		case "interface.mtu":
			s.MTU, err = strconv.Atoi(value)
		case "interface.jc":
			s.Params.Jc, err = strconv.Atoi(value)
		case "interface.jmin":
			s.Params.Jmin, err = strconv.Atoi(value)
		case "interface.jmax":
			s.Params.Jmax, err = strconv.Atoi(value)
		case "interface.s1":
			s.Params.S1, err = strconv.Atoi(value)
		case "interface.s2":
			s.Params.S2, err = strconv.Atoi(value)
		case "interface.s3":
			s.Params.S3, err = strconv.Atoi(value)
		case "interface.s4":
			s.Params.S4, err = strconv.Atoi(value)
		case "interface.headerprotectionkey":
			s.Params.HeaderProtectionKey, err = awg.ParseKeyBase64(value)
		case "interface.randomtrailers":
			s.Params.RandomTrailers, err = parseBool(value)
		case "interface.i1":
			s.Params.I1 = value
		case "peer.publickey":
			s.ServerPublic, err = awg.ParseKeyBase64(value)
		case "peer.allowedips":
			s.AllowedIPs, err = prefixes(value, false)
		case "peer.endpoint":
			s.Endpoint, err = netip.ParseAddrPort(value)
		case "peer.persistentkeepalive":
			s.Keepalive, err = strconv.Atoi(value)
		default:
			return fail("unknown setting %q in [%s]", key, section)
		}
		if err != nil {
			return fail("%s: %v", key, err)
		}
	}

	switch {
	case s.PrivateKey.IsZero():
		return Settings{}, errors.New("no PrivateKey")
	case len(s.Addresses) == 0:
		return Settings{}, errors.New("no Address")
	case peers == 0 || s.ServerPublic.IsZero():
		return Settings{}, errors.New("no [Peer] with a PublicKey")
	case !s.Endpoint.IsValid():
		return Settings{}, errors.New("no Endpoint")
	case len(s.AllowedIPs) == 0:
		return Settings{}, errors.New("no AllowedIPs")
	}
	if s.MTU == 0 {
		s.MTU = awg.MTU
	}
	if err := s.Params.Validate(); err != nil {
		return Settings{}, err
	}
	return s, nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "1", "true", "on", "yes":
		return true, nil
	case "0", "false", "off", "no":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a yes or a no", v)
}

// prefixes reads a comma separated list of CIDR blocks. With bare set, a lone
// address ("10.8.0.7") is accepted as a host.
func prefixes(v string, bare bool) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p)
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil || !bare {
			return nil, fmt.Errorf("%q is not a network", f)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func addrs(v string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address", f)
		}
		out = append(out, a)
	}
	return out, nil
}

// UAPI renders the settings in the form amneziawg-go reads.
func (s Settings) UAPI() string {
	allowed := make([]string, len(s.AllowedIPs))
	for i, p := range s.AllowedIPs {
		allowed[i] = p.String()
	}
	return awg.ServerConfig{
		PrivateKey: s.PrivateKey,
		Params:     s.Params,
		Peers: []awg.Peer{{
			PublicKey:  s.ServerPublic,
			AllowedIPs: allowed,
			Endpoint:   s.Endpoint.String(),
			Keepalive:  s.Keepalive,
		}},
	}.UAPI()
}

// Logger receives what the device has to say. Either may be nil.
type Logger struct {
	Verbosef func(format string, args ...any)
	Errorf   func(format string, args ...any)
}

func (l *Logger) device() *device.Logger {
	dl := &device.Logger{Verbosef: device.DiscardLogf, Errorf: device.DiscardLogf}
	if l == nil {
		return dl
	}
	if l.Verbosef != nil {
		dl.Verbosef = l.Verbosef
	}
	if l.Errorf != nil {
		dl.Errorf = l.Errorf
	}
	return dl
}

// Tunnel is a running client.
type Tunnel struct {
	dev  *device.Device
	bind conn.Bind
}

// Up starts a client on tunDev, which is closed with the tunnel.
func Up(tunDev tun.Device, s Settings, log *Logger) (*Tunnel, error) {
	bind := conn.NewDefaultBind()
	dev := device.NewDevice(tunDev, bind, log.device())
	if err := dev.IpcSet(s.UAPI()); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure the device: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("bring the device up: %w", err)
	}
	return &Tunnel{dev: dev, bind: bind}, nil
}

// Close stops the client and closes its TUN device. Calling it again is harmless.
func (t *Tunnel) Close() {
	if t.dev != nil {
		t.dev.Close()
		t.dev = nil
	}
}

// LatestHandshake is when the server last completed a handshake with this
// client, and whether it has done so at all.
func (t *Tunnel) LatestHandshake() (time.Time, bool) {
	if t.dev == nil {
		return time.Time{}, false
	}
	out, err := t.dev.IpcGet()
	if err != nil {
		return time.Time{}, false
	}
	return parseHandshake(out)
}

func parseHandshake(uapi string) (time.Time, bool) {
	var newest int64
	for _, line := range strings.Split(uapi, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "last_handshake_time_sec=")
		if !ok {
			continue
		}
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil && sec > newest {
			newest = sec
		}
	}
	if newest == 0 {
		return time.Time{}, false
	}
	return time.Unix(newest, 0), true
}

// SocketFDs returns the descriptors of the UDP sockets the client sends its
// packets from, or -1 where there is none. The operating system has to be told
// to keep them out of the tunnel itself.
func (t *Tunnel) SocketFDs() (v4, v6 int) {
	v4, v6 = -1, -1
	peek, ok := t.bind.(interface {
		PeekLookAtSocketFd4() (int, error)
		PeekLookAtSocketFd6() (int, error)
	})
	if !ok {
		return
	}
	if fd, err := peek.PeekLookAtSocketFd4(); err == nil {
		v4 = fd
	}
	if fd, err := peek.PeekLookAtSocketFd6(); err == nil {
		v6 = fd
	}
	return
}
