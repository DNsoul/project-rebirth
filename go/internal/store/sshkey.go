package store

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// generateSSHKey makes the Ed25519 key pair used to reach the servers. The
// private half stays in the state file; the public half is registered with
// Vultr so new instances accept it.
func generateSSHKey() (private string, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate ssh key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, "awg-rotator")
	if err != nil {
		return "", "", fmt.Errorf("encode ssh private key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", fmt.Errorf("encode ssh public key: %w", err)
	}

	return string(pem.EncodeToMemory(block)),
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " awg-rotator",
		nil
}

// SSHSigner turns the stored private key into something the ssh client can
// authenticate with.
func (s *State) SSHSigner() (ssh.Signer, error) {
	if s.SSHPrivate == "" {
		return nil, fmt.Errorf("no ssh key in state")
	}
	signer, err := ssh.ParsePrivateKey([]byte(s.SSHPrivate))
	if err != nil {
		return nil, fmt.Errorf("parse ssh key: %w", err)
	}
	return signer, nil
}
