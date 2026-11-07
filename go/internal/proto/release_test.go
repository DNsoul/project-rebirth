package proto

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

func goodRelease() *Release {
	return &Release{
		Version:  "1.2.0",
		Build:    12,
		MinBuild: 3,
		IssuedAt: testIat,
		Notes:    "что нового",
		Assets: []ReleaseAsset{
			{OS: OSWindows, URL: "https://example.org/Rebirth-Setup-1.2.0.exe", SHA256: strings.Repeat("ab", 32), Size: 1000},
			{OS: OSAndroid, URL: "https://example.org/Rebirth-1.2.0.apk", SHA256: strings.Repeat("cd", 32), Size: 2000},
		},
	}
}

func TestReleaseRoundTrip(t *testing.T) {
	f := newFixture(t)
	blob, err := SealRelease(goodRelease(), IssuerRoot, f.root, testContent)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, next, err := f.trust.OpenRelease(blob, State{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got.Version != "1.2.0" || got.Build != 12 || got.MinBuild != 3 {
		t.Errorf("got %+v", got)
	}
	if win, ok := got.Asset(OSWindows); !ok || win.Size != 1000 {
		t.Errorf("the Windows installer is missing or wrong: %+v", win)
	}
	if _, ok := got.Asset("ios"); ok {
		t.Error("an installer was found for a platform that has none")
	}
	if next.LastBuild != 12 {
		t.Errorf("the newest build seen is %d, want 12", next.LastBuild)
	}
}

func TestBackupKeyMayAnnounceAReleaseToo(t *testing.T) {
	f := newFixture(t)
	blob, err := SealRelease(goodRelease(), IssuerBackup, f.backup, testContent)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.trust.OpenRelease(blob, State{}); err != nil {
		t.Fatalf("a release signed by the backup key was refused: %v", err)
	}
}

func TestReleaseSignedByAStrangerIsRefused(t *testing.T) {
	f := newFixture(t)
	// Signed by the server key, which may sign configurations but not releases,
	// while claiming to come from root.
	r := goodRelease()
	r.V, r.Issuer = FormatVersion, IssuerRoot
	r.Sig = Encode(ed25519.Sign(f.server, r.signedBytes()))
	blob, err := sealJSON(r, testContent)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.trust.OpenRelease(blob, State{}); err == nil {
		t.Fatal("a release signed by the wrong key was accepted")
	}
}

func TestEveryFieldOfAReleaseIsSigned(t *testing.T) {
	f := newFixture(t)
	for name, tamper := range map[string]func(*Release){
		"the version":   func(r *Release) { r.Version = "9.9.9" },
		"the build":     func(r *Release) { r.Build = 99 },
		"the minimum":   func(r *Release) { r.MinBuild = 11 },
		"the date":      func(r *Release) { r.IssuedAt++ },
		"the notes":     func(r *Release) { r.Notes = "другое" },
		"an address":    func(r *Release) { r.Assets[0].URL = "https://evil.example/x.exe" },
		"a hash":        func(r *Release) { r.Assets[1].SHA256 = strings.Repeat("ef", 32) },
		"a size":        func(r *Release) { r.Assets[0].Size++ },
		"a platform":    func(r *Release) { r.Assets[0].OS, r.Assets[1].OS = r.Assets[1].OS, r.Assets[0].OS },
		"the issuer":    func(r *Release) { r.Issuer = IssuerBackup },
		"the installer": func(r *Release) { r.Assets = r.Assets[:1] },
	} {
		r := goodRelease()
		if err := r.Sign(IssuerRoot, f.root); err != nil {
			t.Fatal(err)
		}
		tamper(r)
		blob, err := sealJSON(r, testContent)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.trust.OpenRelease(blob, State{}); err == nil {
			t.Errorf("changing %s after signing was not noticed", name)
		}
	}
}

func TestAnOlderReleaseCannotBeReplayed(t *testing.T) {
	f := newFixture(t)
	old := goodRelease()
	old.Build, old.Version = 11, "1.1.0"
	blob, err := SealRelease(old, IssuerRoot, f.root, testContent)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.trust.OpenRelease(blob, State{LastBuild: 12}); err == nil {
		t.Fatal("build 11 was accepted after build 12 had been seen")
	}
	// The very same build again is only reading the file twice.
	blob, err = SealRelease(goodRelease(), IssuerRoot, f.root, testContent)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.trust.OpenRelease(blob, State{LastBuild: 12}); err != nil {
		t.Fatalf("the same build twice was refused: %v", err)
	}
}

func TestReleaseWithNonsenseIsRefusedAtSigning(t *testing.T) {
	f := newFixture(t)
	for name, mutate := range map[string]func(*Release){
		"a version with letters":    func(r *Release) { r.Version = "latest" },
		"no build":                  func(r *Release) { r.Build = 0 },
		"a minimum above the build": func(r *Release) { r.MinBuild = 13 },
		"no installers":             func(r *Release) { r.Assets = nil },
		"two for one platform":      func(r *Release) { r.Assets[1].OS = OSWindows },
		"an unknown platform":       func(r *Release) { r.Assets[0].OS = "plan9" },
		"a plain http address":      func(r *Release) { r.Assets[0].URL = "http://example.org/a.exe" },
		"a file address":            func(r *Release) { r.Assets[0].URL = "file:///c:/a.exe" },
		"an address with no host":   func(r *Release) { r.Assets[0].URL = "https:///a.exe" },
		"a hash in capitals":        func(r *Release) { r.Assets[0].SHA256 = strings.Repeat("AB", 32) },
		"a short hash":              func(r *Release) { r.Assets[0].SHA256 = "abcd" },
		"an empty file":             func(r *Release) { r.Assets[0].Size = 0 },
		"notes that are too long":   func(r *Release) { r.Notes = strings.Repeat("я", maxReleaseNotes) },
	} {
		r := goodRelease()
		mutate(r)
		if err := r.Sign(IssuerRoot, f.root); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
	if err := goodRelease().Sign("someone", f.root); err == nil {
		t.Error("an unknown issuer was allowed to sign")
	}
}

func TestAReleaseIsNotACommandOrAnythingElse(t *testing.T) {
	f := newFixture(t)
	// The signing domains differ, so a signature made for one kind of document
	// cannot be lifted onto another.
	rel := goodRelease()
	if err := rel.Sign(IssuerRoot, f.root); err != nil {
		t.Fatal(err)
	}
	blob, err := sealJSON(rel, testContent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.trust.OpenCommand(blob, testNow); err == nil {
		t.Error("a release was accepted as a command")
	}
	if _, _, err := f.trust.OpenRevocation(blob, testNow, State{}); err == nil {
		t.Error("a release was accepted as a revocation")
	}
}

func TestReleaseOnAnotherContentKeyNeverOpens(t *testing.T) {
	f := newFixture(t)
	blob, err := SealRelease(goodRelease(), IssuerRoot, f.root, testContent)
	if err != nil {
		t.Fatal(err)
	}
	other := f.trust
	other.ContentKey = Encode(repeat(0x44))
	if _, _, err := other.OpenRelease(blob, State{}); err == nil {
		t.Fatal("opened with the wrong content key")
	}
}
