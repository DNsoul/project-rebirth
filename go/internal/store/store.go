// Package store keeps the operator's local state: the signing keys, the gist
// it publishes to, and what it knows about the current server.
//
// Everything here is secret. The file is written with owner-only permissions,
// which on Windows means it is only as protected as the user profile folder.
package store

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"awgrotator/internal/gitlab"
	"awgrotator/internal/proto"
)

// Version is the on-disk layout version, so a later change can migrate.
const Version = 1

// Keys holds the secret material. Ed25519 keys are stored as their 32-byte
// seed and expanded on load.
type Keys struct {
	// RootSeed signs certificates, commands and revocations. It lives here
	// during development; once the phone becomes the super client it
	// generates its own root key in Keystore and the apps are rebuilt
	// against that one.
	RootSeed string `json:"root_seed"`

	// BackupSeed is the offline key of last resort. Export it and keep it
	// off this machine.
	BackupSeed string `json:"backup_seed"`

	// ServerSeed is the key the current generation of servers signs with.
	ServerSeed string `json:"server_seed"`

	// ContentKey encrypts everything published to the gist and is baked
	// into every app build.
	ContentKey string `json:"content_key"`
}

// Server is what we know about the instance currently in service.
type Server struct {
	ID        string `json:"id,omitempty"`
	Region    string `json:"region,omitempty"`
	IP        string `json:"ip,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

// Mirror is another place the project keeps things besides GitHub: the
// installers of a release, and the same signed files that are in the gist. The
// place itself is a public project and nothing about it is secret: the apps are
// told where it is by the bootstrap data they are built with. An installer is
// only installed if its size and hash are the signed ones, and a file is only
// used if it verifies, so the place needs no trust.
//
// The one secret here is DeployToken, which servers use to write the files.
type Mirror struct {
	Kind      string `json:"kind"`       // "gitlab"
	Base      string `json:"base"`       // https://gitlab.com
	ProjectID int64  `json:"project_id"` // the numeric id, which survives a rename
	Path      string `json:"path"`       // owner/name, for people
	Package   string `json:"package"`    // the generic package the installers go under

	// SyncPackage is the package the signed files of the configuration are kept
	// under (gitlab.DefaultSyncPackage when empty).
	SyncPackage string `json:"sync_package,omitempty"`

	// DeployToken is a token of the project that can only read and write its
	// packages. Servers are given it, so that they can write to this place as
	// they write to the gist.
	DeployToken string `json:"deploy_token,omitempty"`
}

// SyncPkg is the package the signed files are kept under.
func (m Mirror) SyncPkg() string {
	if m.SyncPackage != "" {
		return m.SyncPackage
	}
	return gitlab.DefaultSyncPackage
}

// GitLabMirror returns the GitLab mirror, if there is one.
func (s *State) GitLabMirror() (Mirror, bool) {
	for _, m := range s.Mirrors {
		if m.Kind == KindGitLab {
			return m, true
		}
	}
	return Mirror{}, false
}

// KindGitLab is the one kind of mirror there is so far.
const KindGitLab = "gitlab"

// State is the whole local state file.
type State struct {
	Version       int    `json:"version"`
	CreatedAt     int64  `json:"created_at"`
	Keys          Keys   `json:"keys"`
	Epoch         uint32 `json:"epoch"`
	Seq           uint64 `json:"seq"`
	GistID        string `json:"gist_id"`
	GistOwner     string `json:"gist_owner"`
	SSHPrivate    string `json:"ssh_private_key"`
	SSHPublic     string `json:"ssh_public_key"`
	VultrSSHKeyID string `json:"vultr_ssh_key_id,omitempty"`
	Server        Server `json:"server"`

	// ServerHostKey is the current server's SSH host key, trusted on first
	// use and checked on every connection after that.
	ServerHostKey string `json:"server_host_key,omitempty"`

	// Generation counts the servers built from this machine. Servers that
	// replace themselves count on from there, so it can lag behind the real
	// number; it is a label, not something anything depends on.
	Generation int `json:"generation,omitempty"`

	// Mirrors are the other places the installers are kept.
	Mirrors []Mirror `json:"mirrors,omitempty"`
}

// SetMirror adds a mirror, or replaces the one of the same kind. What was set
// up for the same project before (the sync package and the servers' token) is
// kept unless the new one says otherwise.
func (s *State) SetMirror(m Mirror) {
	for i := range s.Mirrors {
		if s.Mirrors[i].Kind == m.Kind {
			old := s.Mirrors[i]
			if old.ProjectID == m.ProjectID && old.Base == m.Base {
				if m.SyncPackage == "" {
					m.SyncPackage = old.SyncPackage
				}
				if m.DeployToken == "" {
					m.DeployToken = old.DeployToken
				}
			}
			s.Mirrors[i] = m
			return
		}
	}
	s.Mirrors = append(s.Mirrors, m)
}

// ReleaseMirrors are the addresses of the mirrors' copies of an installer as the
// apps are built to know them: `{version}` and `{file}` are filled in by the app
// from the signed announcement.
func (s *State) ReleaseMirrors() []string {
	var out []string
	for _, m := range s.Mirrors {
		if m.Kind == KindGitLab {
			out = append(out, gitlab.PackageTemplate(m.Base, m.ProjectID, m.Package))
		}
	}
	return out
}

// ConfigMirrors are the addresses of the mirrors' copies of the signed files as
// the apps are built to know them: `{file}` is filled in by the app.
func (s *State) ConfigMirrors() []string {
	var out []string
	for _, m := range s.Mirrors {
		if m.Kind == KindGitLab {
			out = append(out, gitlab.SyncTemplate(m.Base, m.ProjectID, m.SyncPkg()))
		}
	}
	return out
}

// Dir returns the state directory inside the project root.
func Dir(root string) string { return filepath.Join(root, "state") }

// Path returns the state file inside the project root.
func Path(root string) string { return filepath.Join(Dir(root), "state.json") }

// Exists reports whether a state file is already present.
func Exists(root string) bool {
	_, err := os.Stat(Path(root))
	return err == nil
}

// New builds a fresh state with newly generated keys.
func New() (*State, error) {
	seeds := make([]string, 3)
	for i := range seeds {
		priv, err := proto.GenerateSigningKey()
		if err != nil {
			return nil, err
		}
		seeds[i] = proto.Encode(priv.Seed())
	}
	content, err := proto.GenerateContentKey()
	if err != nil {
		return nil, err
	}
	sshPriv, sshPub, err := generateSSHKey()
	if err != nil {
		return nil, err
	}

	return &State{
		Version:   Version,
		CreatedAt: time.Now().Unix(),
		Keys: Keys{
			RootSeed:   seeds[0],
			BackupSeed: seeds[1],
			ServerSeed: seeds[2],
			ContentKey: proto.Encode(content),
		},
		// Epoch 1 is the first generation of the server key.
		Epoch:      1,
		Seq:        0,
		SSHPrivate: sshPriv,
		SSHPublic:  sshPub,
	}, nil
}

// Load reads the state file.
func Load(root string) (*State, error) {
	raw, err := os.ReadFile(Path(root))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no state yet, run \"awgctl init\" first")
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state file is not valid JSON: %w", err)
	}
	if s.Version != Version {
		return nil, fmt.Errorf("state file version %d, this build understands %d", s.Version, Version)
	}
	return &s, nil
}

// Save writes the state file, replacing it atomically so an interrupted write
// cannot leave the keys truncated.
func (s *State) Save(root string) error {
	if err := os.MkdirAll(Dir(root), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmp := Path(root) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, Path(root)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s *State) key(seed string, what string) (ed25519.PrivateKey, error) {
	b, err := proto.Decode(seed, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("%s key: %w", what, err)
	}
	return ed25519.NewKeyFromSeed(b), nil
}

// RootKey returns the root signing key.
func (s *State) RootKey() (ed25519.PrivateKey, error) {
	return s.key(s.Keys.RootSeed, "root")
}

// BackupKey returns the offline backup signing key.
func (s *State) BackupKey() (ed25519.PrivateKey, error) {
	return s.key(s.Keys.BackupSeed, "backup")
}

// ServerKey returns the signing key the servers use.
func (s *State) ServerKey() (ed25519.PrivateKey, error) {
	return s.key(s.Keys.ServerSeed, "server")
}

// ContentKey returns the symmetric key for the envelope.
func (s *State) ContentKey() ([]byte, error) {
	return proto.Decode(s.Keys.ContentKey, proto.ContentKeySize)
}

// Trust builds the verifier configuration that gets baked into app builds.
func (s *State) Trust() (proto.Trust, error) {
	root, err := s.RootKey()
	if err != nil {
		return proto.Trust{}, err
	}
	backup, err := s.BackupKey()
	if err != nil {
		return proto.Trust{}, err
	}
	return proto.Trust{
		RootPub:    proto.Encode(proto.PublicOf(root)),
		BackupPub:  proto.Encode(proto.PublicOf(backup)),
		ContentKey: s.Keys.ContentKey,
	}, nil
}

// NewCert issues a certificate for the current server key, valid for the
// given duration.
func (s *State) NewCert(valid time.Duration, now time.Time) (proto.Cert, error) {
	root, err := s.RootKey()
	if err != nil {
		return proto.Cert{}, err
	}
	server, err := s.ServerKey()
	if err != nil {
		return proto.Cert{}, err
	}
	c := proto.Cert{
		Epoch:     s.Epoch,
		NotBefore: now.Unix(),
		NotAfter:  now.Add(valid).Unix(),
		ServerPub: proto.Encode(proto.PublicOf(server)),
	}
	if err := c.Sign(proto.IssuerRoot, root); err != nil {
		return proto.Cert{}, err
	}
	return c, nil
}

// NextSeq bumps and returns the publication counter. The caller must Save.
func (s *State) NextSeq() uint64 {
	s.Seq++
	return s.Seq
}
