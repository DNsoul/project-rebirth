package proto

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"time"
)

const domainRelease = "awg-rotator/release/v1"

// Platforms a release can carry an installer for.
const (
	OSWindows = "windows"
	OSAndroid = "android"
)

// Limits on what a release may say, so that a mistake cannot become a document
// the apps choke on.
const (
	maxReleaseNotes  = 4000
	maxReleaseAssets = 4
	maxAssetSize     = 1 << 31
)

var (
	releaseVersion = regexp.MustCompile(`^\d+\.\d+\.\d+([+-][0-9A-Za-z.-]+)?$`)
	releaseSHA256  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ReleaseAsset is one installer: where to get it and what it must be.
//
// The address is only a place to download from. What makes the file the right
// one is the hash, which is signed with the rest, so a download server can say
// anything it likes and the app will still refuse a file that is not this one.
type ReleaseAsset struct {
	OS     string `json:"os"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"` // lower-case hex of the file's SHA-256
	Size   uint64 `json:"size"`
}

// Release says that a new version of the apps exists and where its installers
// are. It is signed by the root or backup key directly, like a command: the
// people who may announce an update are the people who may command the servers.
type Release struct {
	V       int    `json:"v"`
	Version string `json:"version"` // for people: "1.2.0"
	Build   uint32 `json:"build"`   // for machines: grows with every release

	// MinBuild is the oldest build that still works against the servers. An app
	// older than this is told that the update is not optional.
	MinBuild uint32 `json:"min_build,omitempty"`

	IssuedAt int64          `json:"iat"`
	Notes    string         `json:"notes,omitempty"`
	Assets   []ReleaseAsset `json:"assets"`
	Issuer   string         `json:"by"`
	Sig      string         `json:"sig"`
}

// Asset returns the installer for a platform.
func (r *Release) Asset(os string) (ReleaseAsset, bool) {
	for _, a := range r.Assets {
		if a.OS == os {
			return a, true
		}
	}
	return ReleaseAsset{}, false
}

// check refuses a release that says something no app could act on.
func (r *Release) check() error {
	if !releaseVersion.MatchString(r.Version) {
		return fmt.Errorf("version %q is not of the form 1.2.3", r.Version)
	}
	if r.Build == 0 {
		return fmt.Errorf("build must be at least 1")
	}
	if r.MinBuild > r.Build {
		return fmt.Errorf("min_build %d is above the build %d it belongs to", r.MinBuild, r.Build)
	}
	if len(r.Notes) > maxReleaseNotes {
		return fmt.Errorf("the notes are %d bytes, the limit is %d", len(r.Notes), maxReleaseNotes)
	}
	if len(r.Assets) == 0 || len(r.Assets) > maxReleaseAssets {
		return fmt.Errorf("a release carries 1 to %d installers, this one has %d", maxReleaseAssets, len(r.Assets))
	}
	seen := map[string]bool{}
	for _, a := range r.Assets {
		if a.OS != OSWindows && a.OS != OSAndroid {
			return fmt.Errorf("unknown platform %q", a.OS)
		}
		if seen[a.OS] {
			return fmt.Errorf("two installers for %s", a.OS)
		}
		seen[a.OS] = true
		u, err := url.Parse(a.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("%s: the address must be an https URL, got %q", a.OS, a.URL)
		}
		if !releaseSHA256.MatchString(a.SHA256) {
			return fmt.Errorf("%s: the hash must be 64 lower-case hex digits", a.OS)
		}
		if a.Size == 0 || a.Size > maxAssetSize {
			return fmt.Errorf("%s: the size %d is not plausible", a.OS, a.Size)
		}
	}
	return nil
}

func (r *Release) signedBytes() []byte {
	s := newSigBuf(domainRelease)
	s.lps(r.Version)
	s.u32(r.Build)
	s.u32(r.MinBuild)
	s.i64(r.IssuedAt)
	s.lps(r.Notes)
	s.u32(uint32(len(r.Assets)))
	for _, a := range r.Assets {
		s.lps(a.OS)
		s.lps(a.URL)
		s.lps(a.SHA256)
		s.u64(a.Size)
	}
	s.lps(r.Issuer)
	return s.bytes()
}

// Sign fills in Issuer and Sig. priv must be the root or backup key.
func (r *Release) Sign(issuer string, priv ed25519.PrivateKey) error {
	if issuer != IssuerRoot && issuer != IssuerBackup {
		return fmt.Errorf("unknown issuer %q", issuer)
	}
	if err := r.check(); err != nil {
		return err
	}
	r.V = FormatVersion
	r.Issuer = issuer
	r.Sig = Encode(ed25519.Sign(priv, r.signedBytes()))
	return nil
}

// OpenRelease decrypts and verifies a release announcement taken from the gist.
//
// An announcement older than one already seen is refused, so that replaying an
// earlier, correctly signed one cannot talk an app into an older installer. An
// announcement of the same build is accepted: reading the same file twice is
// not an attack.
func (t *Trust) OpenRelease(blob string, st State) (*Release, State, error) {
	ck, err := t.contentKey()
	if err != nil {
		return nil, st, fmt.Errorf("content key: %w", err)
	}
	plain, err := Open(ck, blob)
	if err != nil {
		return nil, st, err
	}

	var r Release
	if err := json.Unmarshal(plain, &r); err != nil {
		return nil, st, fmt.Errorf("release is not valid JSON: %w", err)
	}
	if r.V != FormatVersion {
		return nil, st, fmt.Errorf("release format version %d, this build understands %d", r.V, FormatVersion)
	}
	if err := r.check(); err != nil {
		return nil, st, fmt.Errorf("release: %w", err)
	}
	key, err := t.issuerKey(r.Issuer)
	if err != nil {
		return nil, st, err
	}
	sig, err := Decode(r.Sig, ed25519.SignatureSize)
	if err != nil {
		return nil, st, fmt.Errorf("release signature: %w", err)
	}
	if !ed25519.Verify(key, r.signedBytes(), sig) {
		return nil, st, fmt.Errorf("release is not signed by the %s key", r.Issuer)
	}
	if r.Build < st.LastBuild {
		return nil, st, fmt.Errorf("release build %d is older than build %d already seen", r.Build, st.LastBuild)
	}

	next := st
	next.LastBuild = r.Build
	return &r, next, nil
}

// SealRelease signs a release with the root or backup key and wraps it for the gist.
func SealRelease(r *Release, issuer string, priv ed25519.PrivateKey, contentKey []byte) (string, error) {
	if err := r.Sign(issuer, priv); err != nil {
		return "", err
	}
	return sealJSON(r, contentKey)
}

// ReleaseAge is how long ago the announcement was made, for display.
func (r *Release) ReleaseAge(now time.Time) time.Duration {
	return now.Sub(time.Unix(r.IssuedAt, 0))
}
