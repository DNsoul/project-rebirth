package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/proto"
	"awgrotator/internal/store"
)

// fakeGist is a gist that lives in a map.
type fakeGist struct {
	files   map[string]string
	updates int
	err     error
}

func (g *fakeGist) Read(ctx context.Context, names ...string) (map[string]string, error) {
	if g.err != nil {
		return nil, g.err
	}
	out := map[string]string{}
	for k, v := range g.files {
		out[k] = v
	}
	return out, nil
}

func (g *fakeGist) Write(ctx context.Context, files map[string]string) error {
	if g.err != nil {
		return g.err
	}
	g.updates++
	for k, v := range files {
		g.files[k] = v
	}
	return nil
}

func writeFile(t *testing.T, dir, name string, size int) (path, sum string) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 7)
	}
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(data)
	return path, hex.EncodeToString(h[:])
}

func goodRequest(t *testing.T) (releaseRequest, string, string) {
	t.Helper()
	dir := t.TempDir()
	exe, exeSum := writeFile(t, dir, "Rebirth-Setup-1.2.0.exe", 5000)
	apk, apkSum := writeFile(t, dir, "Rebirth 1.2.0.apk", 3000)
	return releaseRequest{
		Version: "1.2.0", Build: 12, MinBuild: 3, Notes: "что нового",
		BaseURL: "https://example.org/releases/v1.2.0/",
		Files: []releaseFile{
			{OS: proto.OSWindows, Path: exe},
			{OS: proto.OSAndroid, Path: apk},
		},
		Now: time.Unix(1762400000, 0),
	}, exeSum, apkSum
}

func newState(t *testing.T) *store.State {
	t.Helper()
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	st.GistID = "gist-1"
	return st
}

func TestReleaseHashesTheFilesAndBuildsTheAddresses(t *testing.T) {
	req, exeSum, apkSum := goodRequest(t)
	rel, err := req.release()
	if err != nil {
		t.Fatal(err)
	}
	win, ok := rel.Asset(proto.OSWindows)
	if !ok || win.SHA256 != exeSum || win.Size != 5000 {
		t.Errorf("windows asset = %+v, want sha %s and 5000 bytes", win, exeSum)
	}
	if win.URL != "https://example.org/releases/v1.2.0/Rebirth-Setup-1.2.0.exe" {
		t.Errorf("windows address = %q", win.URL)
	}
	and, _ := rel.Asset(proto.OSAndroid)
	if and.SHA256 != apkSum || and.Size != 3000 {
		t.Errorf("android asset = %+v", and)
	}
	if and.URL != "https://example.org/releases/v1.2.0/Rebirth%201.2.0.apk" {
		t.Errorf("a name with a space must be escaped, got %q", and.URL)
	}
	if rel.IssuedAt != 1762400000 || rel.Build != 12 || rel.MinBuild != 3 {
		t.Errorf("release = %+v", rel)
	}
}

func TestReleaseExplicitAddressWinsOverTheFolder(t *testing.T) {
	req, _, _ := goodRequest(t)
	req.Files[0].URL = "https://cdn.example.org/win.exe"
	rel, err := req.release()
	if err != nil {
		t.Fatal(err)
	}
	if win, _ := rel.Asset(proto.OSWindows); win.URL != "https://cdn.example.org/win.exe" {
		t.Errorf("address = %q", win.URL)
	}
}

func TestReleaseNeedsToKnowWhereFilesAre(t *testing.T) {
	req, _, _ := goodRequest(t)
	req.BaseURL = ""
	if _, err := req.release(); err == nil || !strings.Contains(err.Error(), "base-url") {
		t.Errorf("err = %v, want one about -base-url", err)
	}
}

func TestReleaseNeedsAnInstaller(t *testing.T) {
	req, _, _ := goodRequest(t)
	req.Files = nil
	if _, err := req.release(); err == nil {
		t.Error("an announcement with nothing to install was made")
	}
}

func TestReleaseWithAMissingFileSaysWhichPlatform(t *testing.T) {
	req, _, _ := goodRequest(t)
	req.Files[1].Path = filepath.Join(t.TempDir(), "nope.apk")
	_, err := req.release()
	if err == nil || !strings.Contains(err.Error(), proto.OSAndroid) {
		t.Errorf("err = %v, want one naming android", err)
	}
}

func TestPubspecVersion(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "pubspec.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	v, b, err := readPubspecVersion(write("name: x\r\nversion: 1.2.0+12\r\npublish_to: 'none'\r\n"))
	if err != nil || v != "1.2.0" || b != 12 {
		t.Errorf("got %q, %d, %v", v, b, err)
	}
	for name, body := range map[string]string{
		"no build number":   "version: 1.2.0\n",
		"a build of zero":   "version: 1.2.0+0\n",
		"no version at all": "name: x\n",
		"an indented line":  "  version: 1.2.0+3\n",
	} {
		if _, _, err := readPubspecVersion(write(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := readPubspecVersion(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a file that is not there was accepted")
	}
}

func TestThePubspecOfThisRepositoryIsReadable(t *testing.T) {
	// The release command depends on this file's shape; so does the app's own
	// test that holds lib/core/version.dart to it.
	v, b, err := readPubspecVersion(filepath.Join("..", "..", "..", "app", "pubspec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if v == "" || b == 0 {
		t.Errorf("got %q, %d", v, b)
	}
}

func TestPublishedReleaseOpensTheWayTheAppOpensIt(t *testing.T) {
	st := newState(t)
	req, _, _ := goodRequest(t)
	rel, err := req.release()
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGist{files: map[string]string{}}

	replaced, err := publishRelease(context.Background(), g, st, rel, false)
	if err != nil {
		t.Fatal(err)
	}
	if replaced != nil {
		t.Errorf("nothing was there to replace, but %+v was reported", replaced)
	}
	blob := g.files[proto.FileRelease]
	if blob == "" {
		t.Fatal("nothing was written to the gist")
	}

	trust, err := st.Trust()
	if err != nil {
		t.Fatal(err)
	}
	got, state, err := trust.OpenRelease(blob, proto.State{})
	if err != nil {
		t.Fatalf("the app would refuse it: %v", err)
	}
	if got.Version != "1.2.0" || got.Build != 12 || state.LastBuild != 12 {
		t.Errorf("got %+v, state %+v", got, state)
	}
	if got.Issuer != proto.IssuerRoot {
		t.Errorf("signed by %q, want the root key", got.Issuer)
	}
	if len(g.files) != 1 {
		t.Errorf("other files were touched: %v", g.files)
	}
}

func TestPublishReplacesAnOlderOneAndSaysSo(t *testing.T) {
	st := newState(t)
	g := &fakeGist{files: map[string]string{}}
	req, _, _ := goodRequest(t)
	rel, _ := req.release()
	if _, err := publishRelease(context.Background(), g, st, rel, false); err != nil {
		t.Fatal(err)
	}

	req2, _, _ := goodRequest(t)
	req2.Version, req2.Build = "1.3.0", 13
	rel2, _ := req2.release()
	replaced, err := publishRelease(context.Background(), g, st, rel2, false)
	if err != nil {
		t.Fatal(err)
	}
	if replaced == nil || replaced.Build != 12 {
		t.Errorf("replaced = %+v, want build 12", replaced)
	}
}

func TestPublishRefusesAnOlderBuildThanThePublishedOne(t *testing.T) {
	st := newState(t)
	g := &fakeGist{files: map[string]string{}}
	req, _, _ := goodRequest(t)
	rel, _ := req.release()
	if _, err := publishRelease(context.Background(), g, st, rel, false); err != nil {
		t.Fatal(err)
	}
	before := g.files[proto.FileRelease]

	older, _, _ := goodRequest(t)
	older.Version, older.Build, older.MinBuild = "1.1.0", 11, 0
	rel2, _ := older.release()
	_, err := publishRelease(context.Background(), g, st, rel2, false)
	if err == nil || !strings.Contains(err.Error(), "новее") {
		t.Fatalf("err = %v, want a refusal that says the published one is newer", err)
	}
	if g.files[proto.FileRelease] != before || g.updates != 1 {
		t.Error("the published announcement was touched anyway")
	}

	// Said on purpose, it goes through.
	if _, err := publishRelease(context.Background(), g, st, rel2, true); err != nil {
		t.Fatalf("with force: %v", err)
	}
}

func TestPublishTheSameBuildAgainIsAllowed(t *testing.T) {
	st := newState(t)
	g := &fakeGist{files: map[string]string{}}
	req, _, _ := goodRequest(t)
	rel, _ := req.release()
	for i := 0; i < 2; i++ {
		if _, err := publishRelease(context.Background(), g, st, rel, false); err != nil {
			t.Fatalf("publication %d: %v", i+1, err)
		}
	}
}

func TestPublishIgnoresThePlaceholderAndGarbage(t *testing.T) {
	st := newState(t)
	contentKey, _ := st.ContentKey()
	placeholder, _ := proto.Seal(contentKey, []byte("{}"))
	for name, current := range map[string]string{
		"nothing":     "",
		"placeholder": placeholder,
		"garbage":     "not an envelope at all",
	} {
		g := &fakeGist{files: map[string]string{proto.FileRelease: current}}
		req, _, _ := goodRequest(t)
		rel, _ := req.release()
		replaced, err := publishRelease(context.Background(), g, st, rel, false)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if replaced != nil {
			t.Errorf("%s: reported as replaced: %+v", name, replaced)
		}
	}
}

func TestPublishRefusesWhatNoAppCouldAct(t *testing.T) {
	st := newState(t)
	g := &fakeGist{files: map[string]string{}}
	req, _, _ := goodRequest(t)
	req.BaseURL = "http://example.org/plain/"
	rel, err := req.release()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publishRelease(context.Background(), g, st, rel, false); err == nil {
		t.Error("an announcement with a plain http address was signed")
	}
	if g.updates != 0 {
		t.Error("something was published anyway")
	}
}

func TestPublishPassesOnAFailureOfTheGist(t *testing.T) {
	st := newState(t)
	boom := errors.New("GitHub не отвечает")
	g := &fakeGist{files: map[string]string{}, err: boom}
	req, _, _ := goodRequest(t)
	rel, _ := req.release()
	if _, err := publishRelease(context.Background(), g, st, rel, false); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the gist's own", err)
	}
}

func TestDescribeSaysWhatWillBeAnnounced(t *testing.T) {
	req, _, _ := goodRequest(t)
	rel, _ := req.release()
	text := describeRelease(rel)
	for _, want := range []string{"Версия 1.2.0, сборка 12", "ниже 3", "windows", "android", "что нового", "https://example.org/releases/v1.2.0/"} {
		if !strings.Contains(text, want) {
			t.Errorf("the description lacks %q:\n%s", want, text)
		}
	}
}
