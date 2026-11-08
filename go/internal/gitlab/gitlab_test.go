package gitlab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeGitLab is just enough of GitLab: the project lookup and the generic
// package registry, in memory.
type fakeGitLab struct {
	t          *testing.T
	token      string
	deploy     string // the deploy token the fake issues and accepts for packages
	visibility string
	mu         sync.Mutex
	files      map[string][]byte // request path -> content
	puts       int
}

func newFake(t *testing.T) (*fakeGitLab, *httptest.Server) {
	f := &fakeGitLab{t: t, token: "secret", deploy: "deploy-secret", visibility: "public", files: map[string][]byte{}}
	return f, httptest.NewServer(f)
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/v4/projects/owner%2Fname":
		_, _ = io.WriteString(w, `{"id": 4242, "visibility": "`+f.visibility+`"}`)
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/packages/generic/"):
		if r.Header.Get("PRIVATE-TOKEN") != f.token && r.Header.Get("DEPLOY-TOKEN") != f.deploy {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"401 Unauthorized"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if int64(len(body)) != r.ContentLength {
			f.t.Errorf("content length %d, body %d", r.ContentLength, len(body))
		}
		f.files[r.URL.Path] = body
		f.puts++
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"message":"201 Created"}`)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/packages/generic/"):
		body, ok := f.files[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v4/projects/4242/deploy_tokens":
		if r.Header.Get("PRIVATE-TOKEN") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var in struct {
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Name == "" || strings.Join(in.Scopes, ",") != "read_package_registry,write_package_registry" {
			f.t.Errorf("deploy token asked for %+v", in)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id": 9, "token": "`+f.deploy+`"}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/projects/"):
		// A project that is not there (the package files are matched above).
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"404 Project Not Found"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestResolveProject(t *testing.T) {
	f, srv := newFake(t)
	defer srv.Close()
	c := New(srv.URL, "")

	id, err := c.ResolveProject(context.Background(), "owner/name")
	if err != nil || id != 4242 {
		t.Fatalf("id = %d, err = %v", id, err)
	}

	if _, err := c.ResolveProject(context.Background(), "owner/missing"); err == nil || !strings.Contains(err.Error(), "не найден") {
		t.Errorf("a project that is not there: %v", err)
	}
	for _, bad := range []string{"name", "/owner/name", "owner/name/"} {
		if _, err := c.ResolveProject(context.Background(), bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}

	f.visibility = "private"
	if _, err := c.ResolveProject(context.Background(), "owner/name"); err == nil || !strings.Contains(err.Error(), "не публичный") {
		t.Errorf("a private project must be refused, apps read without a token: %v", err)
	}
}

func TestUploadAndCheck(t *testing.T) {
	f, srv := newFake(t)
	defer srv.Close()
	content := []byte(strings.Repeat("installer bytes ", 5000))
	path := filepath.Join(t.TempDir(), "Rebirth-Setup-1.2.0.exe")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}

	addr, err := New(srv.URL, "secret").UploadFile(context.Background(), 4242, "rebirth", "1.2.0", "Rebirth-Setup-1.2.0.exe", path)
	if err != nil {
		t.Fatal(err)
	}
	want := srv.URL + "/api/v4/projects/4242/packages/generic/rebirth/1.2.0/Rebirth-Setup-1.2.0.exe"
	if addr != want {
		t.Errorf("address = %s, want %s", addr, want)
	}
	if f.puts != 1 {
		t.Errorf("%d uploads", f.puts)
	}

	// The way an app reads it: no token.
	if err := CheckDownload(context.Background(), nil, addr, hashOf(content), uint64(len(content))); err != nil {
		t.Errorf("a good copy was refused: %v", err)
	}
	if err := CheckDownload(context.Background(), nil, addr, hashOf([]byte("other")), uint64(len(content))); err == nil || !strings.Contains(err.Error(), "контрольной суммой") {
		t.Errorf("a wrong hash was accepted: %v", err)
	}
	if err := CheckDownload(context.Background(), nil, addr, hashOf(content), 10); err == nil || !strings.Contains(err.Error(), "байт") {
		t.Errorf("a wrong size was accepted: %v", err)
	}
	if err := CheckDownload(context.Background(), nil, srv.URL+"/api/v4/projects/4242/packages/generic/rebirth/9.9.9/none.exe", "00", 1); err == nil {
		t.Error("a copy that is not there was accepted")
	}
}

func TestUploadNeedsAToken(t *testing.T) {
	_, srv := newFake(t)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "a.exe")
	_ = os.WriteFile(path, []byte("x"), 0o600)

	if _, err := New(srv.URL, "").UploadFile(context.Background(), 1, "rebirth", "1.0.0", "a.exe", path); err == nil || !strings.Contains(err.Error(), "GITLAB_TOKEN") {
		t.Errorf("no token: %v", err)
	}
	if _, err := New(srv.URL, "wrong").UploadFile(context.Background(), 1, "rebirth", "1.0.0", "a.exe", path); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused token should say so: %v", err)
	}
	if _, err := New(srv.URL, "secret").UploadFile(context.Background(), 1, "rebirth", "1.0.0", "missing.exe", filepath.Join(t.TempDir(), "missing.exe")); err == nil {
		t.Error("a missing file was uploaded")
	}
}

func TestAddresses(t *testing.T) {
	if got := PackageURL("https://gitlab.example/", 7, "rebirth", "1.2.0", "Rebirth Setup.exe"); got !=
		"https://gitlab.example/api/v4/projects/7/packages/generic/rebirth/1.2.0/Rebirth%20Setup.exe" {
		t.Errorf("PackageURL = %s", got)
	}
	if got := PackageTemplate("", 7, "rebirth"); got !=
		"https://gitlab.com/api/v4/projects/7/packages/generic/rebirth/{version}/{file}" {
		t.Errorf("PackageTemplate = %s", got)
	}
}

func TestPutAndGetFile(t *testing.T) {
	f, srv := newFake(t)
	defer srv.Close()
	ctx := context.Background()

	if _, found, err := New(srv.URL, "").GetFile(ctx, 4242, "sync", "1", "c"); err != nil || found {
		t.Fatalf("a file that is not there: found=%v err=%v", found, err)
	}

	// The operator's own token and a server's deploy token both write.
	if err := New(srv.URL, "secret").PutFile(ctx, 4242, "sync", "1", "c", []byte("first")); err != nil {
		t.Fatal(err)
	}
	server := New(srv.URL, "deploy-secret")
	server.Deploy = true
	if err := server.PutFile(ctx, 4242, "sync", "1", "c", []byte("second")); err != nil {
		t.Fatalf("a deploy token must be able to write: %v", err)
	}
	if f.puts != 2 {
		t.Errorf("%d uploads", f.puts)
	}

	// An app reads without any token, and sees the newest.
	got, found, err := New(srv.URL, "").GetFile(ctx, 4242, "sync", "1", "c")
	if err != nil || !found || string(got) != "second" {
		t.Errorf("read back %q found=%v err=%v", got, found, err)
	}

	// A deploy token sent as an ordinary one is not accepted, and neither is no
	// token.
	if err := New(srv.URL, "deploy-secret").PutFile(ctx, 4242, "sync", "1", "c", []byte("x")); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a deploy token in the wrong header: %v", err)
	}
	if err := New(srv.URL, "").PutFile(ctx, 4242, "sync", "1", "c", []byte("x")); err == nil {
		t.Error("writing without a token worked")
	}
}

func TestCreateDeployToken(t *testing.T) {
	_, srv := newFake(t)
	defer srv.Close()

	tok, err := New(srv.URL, "secret").CreateDeployToken(context.Background(), 4242, "rebirth servers")
	if err != nil || tok != "deploy-secret" {
		t.Fatalf("token = %q, err = %v", tok, err)
	}
	if _, err := New(srv.URL, "").CreateDeployToken(context.Background(), 4242, "x"); err == nil {
		t.Error("made a token without one")
	}
	if _, err := New(srv.URL, "wrong").CreateDeployToken(context.Background(), 4242, "x"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused token should say so: %v", err)
	}
	asDeploy := New(srv.URL, "deploy-secret")
	asDeploy.Deploy = true
	if _, err := asDeploy.CreateDeployToken(context.Background(), 4242, "x"); err == nil {
		t.Error("a deploy token must not be able to make more tokens")
	}
}

func TestSyncTemplate(t *testing.T) {
	if got := SyncTemplate("", 7, "sync"); got != "https://gitlab.com/api/v4/projects/7/packages/generic/sync/1/{file}" {
		t.Errorf("SyncTemplate = %s", got)
	}
}
