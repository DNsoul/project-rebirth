package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"awgrotator/internal/proto"
	"awgrotator/internal/store"
)

// fakeGitLab keeps package files in memory. It can be told to serve something
// else than what was put in, as a broken or hostile mirror would.
type fakeGitLab struct {
	mu       sync.Mutex
	files    map[string][]byte
	token    string
	tamper   bool
	hideFrom bool // answer downloads without a token with 404, as a private project would
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		if r.Header.Get("PRIVATE-TOKEN") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.files[r.URL.Path] = body
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet:
		if f.hideFrom && r.Header.Get("PRIVATE-TOKEN") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, ok := f.files[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.tamper {
			body = append([]byte("x"), body[1:]...)
		}
		_, _ = w.Write(body)
	}
}

func mirrorFixture(t *testing.T) (*fakeGitLab, *httptest.Server, *store.State, *proto.Release, []releaseFile) {
	t.Helper()
	req, _, _ := goodRequest(t)
	rel, err := req.release()
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitLab{files: map[string][]byte{}, token: "secret"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st := &store.State{}
	st.SetMirror(store.Mirror{Kind: store.KindGitLab, Base: srv.URL, ProjectID: 77, Path: "owner/name", Package: "rebirth"})
	return f, srv, st, rel, req.Files
}

func TestUploadToMirrors(t *testing.T) {
	f, _, st, rel, files := mirrorFixture(t)
	var out bytes.Buffer
	if err := uploadToMirrors(context.Background(), &out, st, "secret", rel, files); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}

	// Each installer is under the name an app will ask for: the last part of the
	// announced address, not whatever the file is called on this machine.
	want := []string{
		"/api/v4/projects/77/packages/generic/rebirth/1.2.0/Rebirth-Setup-1.2.0.exe",
		"/api/v4/projects/77/packages/generic/rebirth/1.2.0/Rebirth%201.2.0.apk",
	}
	for _, path := range want {
		unescaped, _ := pathUnescape(path)
		if _, ok := f.files[unescaped]; !ok {
			t.Errorf("%s was not uploaded; have %v", unescaped, keys(f.files))
		}
	}
	if !strings.Contains(out.String(), "на месте и совпадает") {
		t.Errorf("the check of each copy is not reported:\n%s", out.String())
	}
}

func TestUploadUsesTheAnnouncedName(t *testing.T) {
	f, _, st, rel, files := mirrorFixture(t)
	// The announced address names the file differently from the file on disk.
	rel.Assets[0].URL = "https://example.org/dl/Rebirth-1.2.0-setup.exe"
	if err := uploadToMirrors(context.Background(), io.Discard, st, "secret", rel, files); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.files["/api/v4/projects/77/packages/generic/rebirth/1.2.0/Rebirth-1.2.0-setup.exe"]; !ok {
		t.Errorf("the announced name was not used; have %v", keys(f.files))
	}
}

func TestUploadFailuresStopTheRelease(t *testing.T) {
	t.Run("a token that is refused", func(t *testing.T) {
		_, _, st, rel, files := mirrorFixture(t)
		err := uploadToMirrors(context.Background(), io.Discard, st, "wrong", rel, files)
		if err == nil || !strings.Contains(err.Error(), "owner/name") || !strings.Contains(err.Error(), "401") {
			t.Errorf("the mirror and the refusal should be named: %v", err)
		}
	})
	t.Run("no token at all", func(t *testing.T) {
		_, _, st, rel, files := mirrorFixture(t)
		err := uploadToMirrors(context.Background(), io.Discard, st, "", rel, files)
		if err == nil || !strings.Contains(err.Error(), "GITLAB_TOKEN") {
			t.Errorf("should say which key is missing: %v", err)
		}
	})
	t.Run("a copy that is not what was uploaded", func(t *testing.T) {
		f, _, st, rel, files := mirrorFixture(t)
		f.tamper = true
		err := uploadToMirrors(context.Background(), io.Discard, st, "secret", rel, files)
		if err == nil || !strings.Contains(err.Error(), "контрольной суммой") {
			t.Errorf("a changed copy must be caught: %v", err)
		}
	})
	t.Run("a copy that people without a token cannot get", func(t *testing.T) {
		f, _, st, rel, files := mirrorFixture(t)
		f.hideFrom = true
		err := uploadToMirrors(context.Background(), io.Discard, st, "secret", rel, files)
		if err == nil || !strings.Contains(err.Error(), "без токена") {
			t.Errorf("a private copy is no use to the apps: %v", err)
		}
	})
	t.Run("an address with no file name", func(t *testing.T) {
		_, _, st, rel, files := mirrorFixture(t)
		rel.Assets[0].URL = "https://example.org/"
		if err := uploadToMirrors(context.Background(), io.Discard, st, "secret", rel, files); err == nil {
			t.Error("no name, nothing to upload under")
		}
	})
}

func TestNoMirrorsNoUploads(t *testing.T) {
	req, _, _ := goodRequest(t)
	rel, _ := req.release()
	if err := uploadToMirrors(context.Background(), io.Discard, &store.State{}, "", rel, req.Files); err != nil {
		t.Errorf("nothing to do is not an error: %v", err)
	}
}

func TestSetMirrorChecksTheProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == "/api/v4/projects/owner%2Fname" {
			_, _ = io.WriteString(w, `{"id": 99, "visibility": "public"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	m, err := setMirror(context.Background(), srv.URL+"/", "owner/name", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != store.KindGitLab || m.ProjectID != 99 || m.Path != "owner/name" || m.Package != "rebirth" || m.Base != srv.URL {
		t.Errorf("mirror = %+v", m)
	}
	if _, err := setMirror(context.Background(), srv.URL, "owner/other", "", ""); err == nil {
		t.Error("a project that is not there was accepted")
	}
}

func TestStateKeepsOneMirrorOfAKind(t *testing.T) {
	st := &store.State{}
	st.SetMirror(store.Mirror{Kind: store.KindGitLab, Base: "https://gitlab.com", ProjectID: 1, Path: "a/b", Package: "rebirth"})
	st.SetMirror(store.Mirror{Kind: store.KindGitLab, Base: "https://gitlab.com", ProjectID: 2, Path: "c/d", Package: "rebirth"})
	if len(st.Mirrors) != 1 || st.Mirrors[0].ProjectID != 2 {
		t.Errorf("mirrors = %+v", st.Mirrors)
	}
	if got := st.ReleaseMirrors(); len(got) != 1 ||
		got[0] != "https://gitlab.com/api/v4/projects/2/packages/generic/rebirth/{version}/{file}" {
		t.Errorf("templates = %v", got)
	}
}

func TestBootstrapCarriesTheMirrorsOnlyWhenThereAreSome(t *testing.T) {
	trust := proto.Trust{RootPub: "r", BackupPub: "b", ContentKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}

	st := &store.State{GistID: "g", GistOwner: "o"}
	raw, err := json.Marshal(newBootstrapBundle(trust, st))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "release_mirrors") {
		t.Errorf("an old build's data must not change when there are no mirrors: %s", raw)
	}

	st.SetMirror(store.Mirror{Kind: store.KindGitLab, Base: "https://gitlab.com", ProjectID: 5, Path: "a/b", Package: "rebirth"})
	raw, err = json.Marshal(newBootstrapBundle(trust, st))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		GistID         string   `json:"gist_id"`
		ReleaseMirrors []string `json:"release_mirrors"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.GistID != "g" || len(got.ReleaseMirrors) != 1 ||
		got.ReleaseMirrors[0] != "https://gitlab.com/api/v4/projects/5/packages/generic/rebirth/{version}/{file}" {
		t.Errorf("bundle = %s", raw)
	}
}

func TestStateFileKeepsMirrors(t *testing.T) {
	root := t.TempDir()
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	st.SetMirror(store.Mirror{Kind: store.KindGitLab, Base: "https://gitlab.com", ProjectID: 5, Path: "a/b", Package: "rebirth"})
	if err := st.Save(root); err != nil {
		t.Fatal(err)
	}
	back, err := store.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Mirrors) != 1 || back.Mirrors[0].ProjectID != 5 {
		t.Errorf("after a round trip: %+v", back.Mirrors)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "state.json")); err != nil {
		t.Error(err)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func pathUnescape(s string) (string, error) {
	return strings.NewReplacer("%20", " ").Replace(s), nil
}
