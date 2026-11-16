package cherry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("key")
	c.base = srv.URL
	return c
}

func TestServerAddressesAndReadiness(t *testing.T) {
	s := Server{Status: "deployed", IPs: []IPAddress{
		{Address: "10.0.0.5", AddressFamily: 4, Type: "private-ip"},
		{Address: "203.0.113.9", AddressFamily: 4, Type: "primary-ip"},
		{Address: "2001:db8::9", AddressFamily: 6, Type: "primary-ip"},
	}}
	if s.PublicIPv4() != "203.0.113.9" || s.PublicIPv6() != "2001:db8::9" {
		t.Errorf("addresses = %q %q", s.PublicIPv4(), s.PublicIPv6())
	}
	if !s.Ready() {
		t.Error("a deployed server with an address is not ready")
	}

	s.Status = "deploying"
	if s.Ready() {
		t.Error("a server still deploying is ready")
	}
	private := Server{Status: "deployed", IPs: []IPAddress{{Address: "10.0.0.5", AddressFamily: 4, Type: "private-ip"}}}
	if private.Ready() {
		t.Error("a server with only a private address is ready")
	}
}

func TestCreateServerSendsTheRightFields(t *testing.T) {
	var got CreateRequest
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/77/servers" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer key" {
			t.Errorf("Authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":555,"status":"pending","project":{"id":77}}`))
	})

	s, err := c.CreateServer(context.Background(), 77, CreateRequest{
		Plan: "B2-1-1gb-20s-shared", Region: "NL-Amsterdam", Image: "debian_13_64bit",
		Hostname: "awg-ams", SSHKeys: []string{"9"}, Tags: map[string]string{"awg-rotator": "production"},
		Cycle: "monthly", ConfigureIPv6: true,
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if s.ID != 555 || s.Project.ID != 77 {
		t.Errorf("server = %+v", s)
	}
	if got.Region != "NL-Amsterdam" || got.Cycle != "monthly" || got.Image != "debian_13_64bit" || !got.ConfigureIPv6 {
		t.Errorf("request = %+v", got)
	}
	if got.Tags["awg-rotator"] != "production" {
		t.Errorf("tags = %v", got.Tags)
	}
}

func TestServersLooksOnlyInsideTheProject(t *testing.T) {
	var path string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`[{"id":1,"status":"deployed","project":{"id":77},"tags":{"awg-rotator":"production"}}]`))
	})
	got, err := c.Servers(context.Background(), 77)
	if err != nil {
		t.Fatalf("Servers: %v", err)
	}
	if path != "/projects/77/servers" {
		t.Errorf("path = %q", path)
	}
	if len(got) != 1 || got[0].Tags["awg-rotator"] != "production" {
		t.Errorf("servers = %+v", got)
	}
}

func TestMissingServerIsNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":404,"message":"Server not found"}`))
	})
	_, err := c.Server(context.Background(), 9)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "Server not found") {
		t.Errorf("error does not carry the message: %v", err)
	}
}

func TestErrorIsNotNotFound(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"message":"Access denied"}`))
	})
	err := c.DeleteServer(context.Background(), 9)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeleteAcceptsEmptyBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/servers/9" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteServer(context.Background(), 9); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
}

func TestProjectsAndKeys(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/teams":
			_, _ = w.Write([]byte(`[{"id":3,"name":"Mine"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/teams/3/projects":
			_, _ = w.Write([]byte(`{"id":77,"name":"awg-rotator"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/ssh-keys":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["label"] != "awg-rotator" || !strings.HasPrefix(in["key"], "ssh-ed25519 ") {
				t.Errorf("key request = %v", in)
			}
			_, _ = w.Write([]byte(`{"id":9}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	ctx := context.Background()
	teams, err := c.Teams(ctx)
	if err != nil || len(teams) != 1 || teams[0].ID != 3 {
		t.Fatalf("Teams = %+v, %v", teams, err)
	}
	p, err := c.CreateProject(ctx, 3, "awg-rotator")
	if err != nil || p.ID != 77 {
		t.Fatalf("CreateProject = %+v, %v", p, err)
	}
	id, err := c.CreateSSHKey(ctx, "awg-rotator", "ssh-ed25519 AAAA")
	if err != nil || id != 9 {
		t.Fatalf("CreateSSHKey = %d, %v", id, err)
	}
}
