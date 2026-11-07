package gist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("tok")
	c.base = srv.URL
	return c
}

func TestCreateMakesSecretGist(t *testing.T) {
	var got gistBody
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/gists" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer tok" {
			t.Errorf("Authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"gid123","owner":{"login":"someone"}}`))
	})

	id, owner, err := c.Create(context.Background(), map[string]string{"c": "blob"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "gid123" || owner != "someone" {
		t.Errorf("got id %q owner %q", id, owner)
	}
	if got.Public {
		t.Error("the gist was created public")
	}
	if got.Description != "" {
		t.Errorf("description = %q, want empty so the link gives nothing away", got.Description)
	}
	if got.Files["c"].Content != "blob" {
		t.Errorf("file content = %q", got.Files["c"].Content)
	}
}

func TestGetReturnsFiles(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gists/gid123" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"gid123","files":{"c":{"content":"one"},"s":{"content":"two"}}}`))
	})

	files, err := c.Get(context.Background(), "gid123")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if files["c"] != "one" || files["s"] != "two" {
		t.Errorf("files = %v", files)
	}
}

func TestTruncatedFileIsRefused(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"files":{"c":{"content":"part","truncated":true}}}`))
	})
	if _, err := c.Get(context.Background(), "gid"); err == nil {
		t.Fatal("a truncated file was accepted")
	}
}

func TestErrorCarriesGitHubMessage(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	_, err := c.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("a 404 was treated as success")
	}
	if !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("error does not carry the message: %v", err)
	}
}

func TestUpdateSendsOnlyNamedFiles(t *testing.T) {
	var body struct {
		Files map[string]gistFile `json:"files"`
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		_, _ = w.Write([]byte(`{}`))
	})

	if err := c.Update(context.Background(), "gid", map[string]string{"c": "new"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(body.Files) != 1 || body.Files["c"].Content != "new" {
		t.Errorf("files = %v", body.Files)
	}
}
