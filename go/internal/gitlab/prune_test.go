package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRegistry answers the package listing calls: one package with several
// uploads of a few files.
type fakeRegistry struct {
	mu      sync.Mutex
	files   []packageFile
	deleted []int64
	pages   int
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("PRIVATE-TOKEN") != "secret" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/7/packages":
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 50, "name": "sync-old", "version": "1"}, // the name is only a similar one
			{"id": 51, "name": "sync", "version": "2"},     // another version
			{"id": 52, "name": "sync", "version": "1"},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/7/packages/52/package_files":
		f.pages++
		if r.URL.Query().Get("page") != "1" {
			_ = json.NewEncoder(w).Encode([]packageFile{})
			return
		}
		_ = json.NewEncoder(w).Encode(f.files)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v4/projects/7/packages/52/package_files/"):
		var id int64
		_, _ = fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "%d", &id)
		f.deleted = append(f.deleted, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestPruneKeepsTheNewestUploadsOfEachFile(t *testing.T) {
	reg := &fakeRegistry{files: []packageFile{
		{ID: 1, FileName: "c"}, {ID: 2, FileName: "c"}, {ID: 5, FileName: "c"}, {ID: 9, FileName: "c"},
		{ID: 3, FileName: "s"}, {ID: 4, FileName: "s"},
		{ID: 6, FileName: "u"},
	}}
	srv := httptest.NewServer(reg)
	defer srv.Close()

	n, err := New(srv.URL, "secret").PruneFiles(context.Background(), 7, "sync", "1", 2)
	if err != nil {
		t.Fatal(err)
	}
	// c has four uploads, so its two oldest (1 and 2) go; s and u have no more
	// than two and stay as they are.
	if n != 2 || len(reg.deleted) != 2 {
		t.Fatalf("deleted %d (%v), want the two oldest of c", n, reg.deleted)
	}
	for _, id := range reg.deleted {
		if id != 1 && id != 2 {
			t.Errorf("deleted %d, which is one of the newest", id)
		}
	}

	if _, err := New(srv.URL, "secret").PruneFiles(context.Background(), 7, "sync", "1", 0); err == nil {
		t.Error("keeping nothing was allowed")
	}
	deploy := New(srv.URL, "x")
	deploy.Deploy = true
	if _, err := deploy.PruneFiles(context.Background(), 7, "sync", "1", 2); err == nil {
		t.Error("a deploy token was allowed to delete")
	}
	if _, err := New(srv.URL, "").PruneFiles(context.Background(), 7, "sync", "1", 2); err == nil {
		t.Error("pruning without a token")
	}
	if _, err := New(srv.URL, "wrong").PruneFiles(context.Background(), 7, "sync", "1", 2); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused token should say so: %v", err)
	}
}

func TestPruneWithNoSuchPackageDoesNothing(t *testing.T) {
	reg := &fakeRegistry{}
	srv := httptest.NewServer(reg)
	defer srv.Close()

	n, err := New(srv.URL, "secret").PruneFiles(context.Background(), 7, "other", "1", 2)
	if err != nil || n != 0 {
		t.Errorf("deleted %d, err %v", n, err)
	}
}
