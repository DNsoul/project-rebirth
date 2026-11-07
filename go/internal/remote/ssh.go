// Package remote runs commands and uploads files on a server over SSH.
//
// Host keys are trusted on first use. A freshly created instance generates
// its key at first boot, so there is nothing to compare against until we have
// connected once; the key is recorded then and checked on every later
// connection. That leaves a window at the very first connection where a
// man in the middle could not be detected, which is the same trade every
// provisioning tool makes.
package remote

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// DialTimeout is how long a single connection attempt may take.
const DialTimeout = 15 * time.Second

// Client is a connection to one server.
type Client struct {
	conn    *ssh.Client
	hostKey string
}

// Dial connects to host:22 as the given user.
//
// knownKey is the authorized_keys-style host key recorded earlier, or empty
// to accept and record whatever the server presents. HostKey reports what was
// seen, so the caller can store it.
func Dial(ctx context.Context, host, user string, signer ssh.Signer, knownKey string) (*Client, error) {
	var seen string

	cfg := &ssh.ClientConfig{
		User:    user,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout: DialTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if knownKey == "" {
				return nil
			}
			if seen != strings.TrimSpace(knownKey) {
				return fmt.Errorf("host key changed: expected %s, got %s",
					fingerprint(knownKey), ssh.FingerprintSHA256(key))
			}
			return nil
		},
	}

	addr := net.JoinHostPort(host, "22")
	dialer := net.Dialer{Timeout: DialTimeout}
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(netConn, addr, cfg)
	if err != nil {
		netConn.Close()
		return nil, err
	}
	return &Client{conn: ssh.NewClient(sshConn, chans, reqs), hostKey: seen}, nil
}

// WaitForSSH dials until the server answers or the context ends. A new
// instance takes a little while to start accepting connections.
func WaitForSSH(ctx context.Context, host, user string, signer ssh.Signer, knownKey string) (*Client, error) {
	var lastErr error
	for {
		c, err := Dial(ctx, host, user, signer, knownKey)
		if err == nil {
			return c, nil
		}
		lastErr = err

		// A changed host key is never going to fix itself by retrying.
		if strings.Contains(err.Error(), "host key changed") {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ssh to %s did not come up: %w", host, lastErr)
		case <-time.After(3 * time.Second):
		}
	}
}

// HostKey returns the key the server presented, in authorized_keys form.
func (c *Client) HostKey() string { return c.hostKey }

// Close hangs up.
func (c *Client) Close() error { return c.conn.Close() }

// Run executes a command and returns its combined output.
func (c *Client) Run(cmd string) (string, error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	// Separate buffers: the ssh package copies the two streams in their own
	// goroutines, and a bytes.Buffer shared between them would be a race.
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	err = session.Run(cmd)
	text := strings.TrimSpace(stdout.String())
	errText := strings.TrimSpace(stderr.String())
	if err != nil {
		return joinOutput(text, errText), fmt.Errorf("%s: %w: %s", firstWord(cmd), err, errText)
	}
	return joinOutput(text, errText), nil
}

func joinOutput(stdout, stderr string) string {
	switch {
	case stderr == "":
		return stdout
	case stdout == "":
		return stderr
	default:
		return stdout + "\n" + stderr
	}
}

// Upload writes content to path with the given mode.
//
// It writes to a temporary name and renames, so a half-transferred file is
// never left where something might execute it.
func (c *Client) Upload(path string, mode string, content []byte) error {
	session, err := c.conn.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	session.Stdin = bytes.NewReader(content)
	var out bytes.Buffer
	session.Stdout = &out
	session.Stderr = &out

	tmp := path + ".part"
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && chmod %s %s && mv %s %s",
		shellQuote(parentDir(path)), shellQuote(tmp), mode, shellQuote(tmp),
		shellQuote(tmp), shellQuote(path))

	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("upload %s: %w: %s", path, err, strings.TrimSpace(out.String()))
	}
	return nil
}

func parentDir(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return "/"
}

// shellQuote wraps a value in single quotes for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

func fingerprint(authorizedKey string) string {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return "(unreadable)"
	}
	return ssh.FingerprintSHA256(key)
}
