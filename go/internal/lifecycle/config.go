// Package lifecycle is what lets a server look after itself: watch its own
// health, accept instructions from the operator, and build, test and hand over
// to its own replacement.
//
// The decisions are kept apart from the things they act on. Whether to move is
// a pure function of the clock, the server's age and what it has observed;
// moving is an engine that talks to the cloud, the installer, the tunnel test
// and the gist only through small interfaces. That separation is what makes
// the dangerous parts, which spend money and delete servers, testable without
// spending or deleting anything.
package lifecycle

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"awgrotator/internal/proto"
)

// ConfigVersion is the on-disk layout version of the manager's configuration.
const ConfigVersion = 1

// Where the manager keeps things on a server.
const (
	DefaultConfigPath = "/etc/awgnode/manager.json"
	DefaultDataDir    = "/var/lib/awgmgr"
	DefaultRulesDir   = "/etc/awgnode"
)

// Policy is when the server may move and how often. Everything that can spend
// money has a ceiling.
type Policy struct {
	// AutoAge moves the server when it gets old, and AutoBlock when it looks
	// blocked or broken. Both are off by default: until the apps can follow a
	// move by themselves, each one means importing a new configuration by hand,
	// so moves happen only when the operator asks.
	AutoAge   bool `json:"auto_age"`
	AutoBlock bool `json:"auto_block"`

	// MaxAgeHours is how long a server lives before AutoAge replaces it, and
	// AgeJitterHours the random extra, drawn once, so that a fleet of
	// replacements does not move on a perfectly regular schedule.
	MaxAgeHours    int `json:"max_age_hours"`
	AgeJitterHours int `json:"age_jitter_hours"`

	// MaxServers is the most instances carrying our tag at any moment. A move
	// needs room for one more, so this is also the ceiling on cost.
	MaxServers int `json:"max_servers"`

	// MaxMovesPerDay and MinMoveMinutes limit automatic attempts, successful
	// or not. An operator's command is exempt from both.
	MaxMovesPerDay int `json:"max_moves_per_day"`
	MinMoveMinutes int `json:"min_move_minutes"`

	// GraceMinutes is how long the old server stays after the new one is
	// published, and MinGraceMinutes the shortest it may leave early, so that
	// clients have time to notice.
	GraceMinutes    int `json:"grace_minutes"`
	MinGraceMinutes int `json:"min_grace_minutes"`

	// BlockRounds is how many external checks in a row must say "blocked"
	// before the server believes it.
	BlockRounds int `json:"block_rounds"`
}

// DefaultPolicy returns the policy a new server starts with.
func DefaultPolicy() Policy {
	return Policy{
		MaxAgeHours:     29 * 24,
		AgeJitterHours:  3 * 24,
		MaxServers:      3,
		MaxMovesPerDay:  3,
		MinMoveMinutes:  6 * 60,
		GraceMinutes:    15,
		MinGraceMinutes: 5,
		BlockRounds:     3,
	}
}

// Normalize replaces zero values with defaults and rejects nonsense.
func (p *Policy) Normalize() error {
	d := DefaultPolicy()
	fill := func(v *int, def int) {
		if *v == 0 {
			*v = def
		}
	}
	fill(&p.MaxAgeHours, d.MaxAgeHours)
	fill(&p.MaxServers, d.MaxServers)
	fill(&p.MaxMovesPerDay, d.MaxMovesPerDay)
	fill(&p.MinMoveMinutes, d.MinMoveMinutes)
	fill(&p.GraceMinutes, d.GraceMinutes)
	fill(&p.MinGraceMinutes, d.MinGraceMinutes)
	fill(&p.BlockRounds, d.BlockRounds)

	switch {
	case p.AgeJitterHours < 0:
		return errors.New("age jitter must not be negative")
	case p.MaxServers < 2:
		// One server cannot build another and still be a server.
		return fmt.Errorf("max servers is %d: a move needs room for at least two", p.MaxServers)
	case p.MaxServers > 6:
		return fmt.Errorf("max servers is %d: refusing a ceiling that high on money", p.MaxServers)
	case p.MinGraceMinutes > p.GraceMinutes:
		return errors.New("the minimum grace period is longer than the grace period")
	case p.MaxMovesPerDay > 12:
		return fmt.Errorf("max moves per day is %d: refusing a ceiling that high", p.MaxMovesPerDay)
	}
	return nil
}

// MaxAge returns the age limit as a duration.
func (p Policy) MaxAge() time.Duration { return time.Duration(p.MaxAgeHours) * time.Hour }

// MinMoveInterval returns the pause between automatic attempts.
func (p Policy) MinMoveInterval() time.Duration {
	return time.Duration(p.MinMoveMinutes) * time.Minute
}

// Grace returns how long the old server lingers.
func (p Policy) Grace() time.Duration { return time.Duration(p.GraceMinutes) * time.Minute }

// MinGrace returns the shortest the old server lingers.
func (p Policy) MinGrace() time.Duration { return time.Duration(p.MinGraceMinutes) * time.Minute }

// Config is everything a server needs to look after itself. It holds
// credentials, so it is written with owner-only permissions.
type Config struct {
	Version int `json:"version"`

	// Identity of this server.
	InstanceID string `json:"instance_id"`
	IP         string `json:"ip"`
	Region     string `json:"region"`
	CreatedAt  int64  `json:"created_at"`

	// Generation counts servers in the line, starting at one.
	Generation int `json:"generation"`

	// Credentials. The Vultr key belongs to a sub-user limited to compute,
	// and the GitHub token to gists only.
	VultrKey    string `json:"vultr_key"`
	GitHubToken string `json:"github_token"`
	GistID      string `json:"gist_id"`

	// GitLab is the second place everything is published to, equal to the gist
	// and not behind it. Without it only the gist is used.
	GitLab *GitLabSync `json:"gitlab,omitempty"`

	// Telegram is the operator's bot. The server that is published runs it;
	// every replacement inherits it.
	Telegram *TelegramConfig `json:"telegram,omitempty"`

	// ServerSeed is the key this line of servers signs with, and Cert is the
	// certificate the operator's root key issued for it. A server cannot make
	// its own certificate, so every replacement inherits this one.
	ServerSeed string     `json:"server_seed"`
	Cert       proto.Cert `json:"cert"`

	// What the apps are built with, needed to read the operator's commands.
	ContentKey string `json:"content_key"`
	RootPub    string `json:"root_pub"`
	BackupPub  string `json:"backup_pub"`

	// SSHPrivate reaches new servers, and SSHKeyID is its registration with
	// the cloud so that new servers accept it.
	SSHPrivate string `json:"ssh_private_key"`
	SSHKeyID   string `json:"ssh_key_id"`

	// What a new server is made from.
	Plan    string   `json:"plan"`
	OsID    int      `json:"os_id"`
	Tag     string   `json:"tag"`
	Regions []string `json:"regions"`

	Policy Policy `json:"policy"`

	// DataDir overrides DefaultDataDir.
	DataDir string `json:"data_dir,omitempty"`
}

// GitLabSync says where the second place is and how to write to it. The token
// is a deploy token of the project: it can read and write that project's package
// registry and reaches nothing else, which is what makes it fit to be left on a
// server that is replaced every few weeks.
type GitLabSync struct {
	Base    string `json:"base"`
	Project int64  `json:"project"`
	Package string `json:"package"`
	Token   string `json:"token"`
}

// TelegramConfig is the operator's bot: its token, and who it answers. Anyone
// else gets no answer at all.
type TelegramConfig struct {
	Token  string  `json:"token"`
	Owners []int64 `json:"owners"`

	// Approval makes new devices wait for the operator, on this server's
	// replacements as on this one. It lives here because without the bot
	// nobody could approve anything.
	Approval bool `json:"approval,omitempty"`
}

func (t *TelegramConfig) validate() error {
	switch {
	case t.Token == "":
		return errors.New("telegram: no bot token")
	case len(t.Owners) == 0:
		return errors.New("telegram: nobody to answer")
	}
	for _, id := range t.Owners {
		if id <= 0 {
			return fmt.Errorf("telegram: %d is not a person's id", id)
		}
	}
	return nil
}

// Approval reports whether new devices wait for the operator.
func (c *Config) Approval() bool { return c.Telegram != nil && c.Telegram.Approval }

func (g *GitLabSync) validate() error {
	switch {
	case !strings.HasPrefix(g.Base, "https://"):
		return fmt.Errorf("gitlab: the address %q is not an https one", g.Base)
	case g.Project <= 0:
		return errors.New("gitlab: no project id")
	case g.Package == "":
		return errors.New("gitlab: no package name")
	case g.Token == "":
		return errors.New("gitlab: no token")
	}
	return nil
}

// Validate checks that the configuration is complete and consistent.
func (c *Config) Validate() error {
	if c.Version != ConfigVersion {
		return fmt.Errorf("manager configuration version %d, this build understands %d", c.Version, ConfigVersion)
	}
	for name, value := range map[string]string{
		"instance id": c.InstanceID, "region": c.Region, "vultr key": c.VultrKey,
		"github token": c.GitHubToken, "gist id": c.GistID, "server seed": c.ServerSeed,
		"content key": c.ContentKey, "root key": c.RootPub, "backup key": c.BackupPub,
		"ssh key": c.SSHPrivate, "ssh key id": c.SSHKeyID, "plan": c.Plan, "tag": c.Tag,
	} {
		if value == "" {
			return fmt.Errorf("manager configuration has no %s", name)
		}
	}
	if c.OsID == 0 {
		return errors.New("manager configuration has no os id")
	}
	if len(c.Regions) < 2 {
		return errors.New("manager configuration needs at least two regions to move between")
	}
	if c.Generation < 1 {
		return errors.New("manager configuration has no generation number")
	}
	if c.CreatedAt <= 0 {
		return errors.New("manager configuration has no creation time")
	}

	seed, err := proto.Decode(c.ServerSeed, ed25519.SeedSize)
	if err != nil {
		return fmt.Errorf("server seed: %w", err)
	}
	// The certificate has to be for the key we hold, or everything we publish
	// would be rejected by every client.
	key := ed25519.NewKeyFromSeed(seed)
	if got := proto.Encode(proto.PublicOf(key)); got != c.Cert.ServerPub {
		return errors.New("the certificate is for a different server key than the one held")
	}
	if _, err := ssh.ParsePrivateKey([]byte(c.SSHPrivate)); err != nil {
		return fmt.Errorf("ssh key: %w", err)
	}
	if c.GitLab != nil {
		if err := c.GitLab.validate(); err != nil {
			return err
		}
	}
	if c.Telegram != nil {
		if err := c.Telegram.validate(); err != nil {
			return err
		}
	}
	return c.Policy.Normalize()
}

// Data returns the directory for the manager's own files.
func (c *Config) Data() string {
	if c.DataDir != "" {
		return c.DataDir
	}
	return DefaultDataDir
}

// ServerKey returns the signing key.
func (c *Config) ServerKey() (ed25519.PrivateKey, error) {
	seed, err := proto.Decode(c.ServerSeed, ed25519.SeedSize)
	if err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ContentKeyBytes returns the envelope key.
func (c *Config) ContentKeyBytes() ([]byte, error) {
	return proto.Decode(c.ContentKey, proto.ContentKeySize)
}

// Trust returns what is needed to verify the operator's documents.
func (c *Config) Trust() proto.Trust {
	return proto.Trust{RootPub: c.RootPub, BackupPub: c.BackupPub, ContentKey: c.ContentKey}
}

// SSHSigner returns the key for reaching new servers.
func (c *Config) SSHSigner() (ssh.Signer, error) {
	return ssh.ParsePrivateKey([]byte(c.SSHPrivate))
}

// CreatedTime returns when this server's life began.
func (c *Config) CreatedTime() time.Time { return time.Unix(c.CreatedAt, 0) }

// CertDaysLeft says how long the certificate stays valid. When it reaches
// zero the line of servers can no longer publish and needs the operator.
func (c *Config) CertDaysLeft(now time.Time) int {
	left := time.Unix(c.Cert.NotAfter, 0).Sub(now)
	if left < 0 {
		return 0
	}
	return int(left / (24 * time.Hour))
}

// Marshal renders the configuration for writing to a server.
func (c *Config) Marshal() ([]byte, error) {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// ParseConfig reads and validates a configuration held in memory.
func ParseConfig(raw []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("the manager configuration is not valid JSON: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// LoadConfig reads and validates the configuration file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := ParseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Save writes the configuration with owner-only permissions, atomically.
func (c *Config) Save(path string) error {
	raw, err := c.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
