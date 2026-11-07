package vultr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("key")
	c.base = srv.URL
	return c
}

func TestInstanceReady(t *testing.T) {
	cases := []struct {
		name string
		i    Instance
		want bool
	}{
		{"running", Instance{Status: "active", PowerStatus: "running", MainIP: "203.0.113.9"}, true},
		{"no address yet", Instance{Status: "active", PowerStatus: "running", MainIP: "0.0.0.0"}, false},
		{"still installing", Instance{Status: "pending", PowerStatus: "running", MainIP: "203.0.113.9"}, false},
		{"stopped", Instance{Status: "active", PowerStatus: "stopped", MainIP: "203.0.113.9"}, false},
	}
	for _, tc := range cases {
		if got := tc.i.Ready(); got != tc.want {
			t.Errorf("%s: Ready() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestInstancesByTagFiltersServerSide(t *testing.T) {
	var gotTag string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotTag = r.URL.Query().Get("tag")
		_, _ = w.Write([]byte(`{"instances":[{"id":"i1","region":"sto","main_ip":"203.0.113.9"}]}`))
	})

	got, err := c.InstancesByTag(context.Background(), "awg rotator")
	if err != nil {
		t.Fatalf("InstancesByTag: %v", err)
	}
	if gotTag != "awg rotator" {
		t.Errorf("tag = %q", gotTag)
	}
	if len(got) != 1 || got[0].ID != "i1" {
		t.Errorf("instances = %+v", got)
	}
}

func TestCreateInstanceSendsTheRightFields(t *testing.T) {
	var got CreateRequest
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/instances" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer key" {
			t.Errorf("Authorization = %q", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"instance":{"id":"new1","status":"pending"}}`))
	})

	inst, err := c.CreateInstance(context.Background(), CreateRequest{
		Region: "sto", Plan: "vc2-1c-1gb", OsID: 2625,
		Tags: []string{"awg-rotator"}, SSHKeys: []string{"k1"},
		UserData: "Zm9v", EnableIPv6: true,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if inst.ID != "new1" {
		t.Errorf("id = %q", inst.ID)
	}
	if got.Region != "sto" || got.OsID != 2625 || !got.EnableIPv6 {
		t.Errorf("request = %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "awg-rotator" {
		t.Errorf("tags = %v", got.Tags)
	}
}

func TestErrorCarriesVultrMessage(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not authorized","status":403}`))
	})
	_, err := c.Instances(context.Background())
	if err == nil {
		t.Fatal("a 403 was treated as success")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("error does not carry the message: %v", err)
	}
}

func TestDeleteAcceptsEmptyBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteInstance(context.Background(), "i1"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}

func TestWaitReadyReturnsAsSoonAsItIsUp(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"instance":{"id":"i1","status":"active","power_status":"running","main_ip":"203.0.113.9"}}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	inst, err := c.WaitReady(ctx, "i1")
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if inst.MainIP != "203.0.113.9" {
		t.Errorf("ip = %q", inst.MainIP)
	}
}

func TestWaitReadyGivesUpWithTheContext(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"instance":{"id":"i1","status":"pending"}}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := c.WaitReady(ctx, "i1"); err == nil {
		t.Fatal("WaitReady returned success for an instance that never came up")
	}
}
