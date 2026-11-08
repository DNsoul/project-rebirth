package gitlab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	visibility string
	mu         sync.Mutex
	files      map[string][]byte // request path -> content
	puts       int
}

func newFake(t *testing.T) (*fakeGitLab, *httptest.Server) {
	f := &fakeGitLab{t: t, token: "secret", visibility: "public", files: map[string][]byte{}}
	return f, httptest.NewServer(f)
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/v4/projects/owner%2Fname":
		_, _ = io.WriteString(w, `{"id": 4242, "visibility": "`+f.visibility+`"}`)
	case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/packages/generic/"):
		if r.Header.Get("PRIVATE-TOKEN") != f.token {
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
