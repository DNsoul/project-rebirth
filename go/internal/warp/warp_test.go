package warp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"awgrotator/internal/awg"
)

// A registration answer of the shape the WARP service gives (trimmed to what
// matters, values made up).
const sampleAnswer = `{
  "id": "t.11111111-2222-3333-4444-555555555555",
  "type": "a",
  "model": "PC",
  "token": "tok-123",
  "account": {"id": "acc", "account_type": "free"},
  "config": {
    "client_id": "AbCd",
    "peers": [{
      "public_key": "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
      "endpoint": {"v4": "162.159.192.7:0", "v6": "[2606:4700:d0::a29f:c007]:0",
                   "host": "engage.cloudflareclient.com:2408", "ports": [2408, 500, 1701, 4500]}
    }],
    "interface": {"addresses": {"v4": "172.16.0.2", "v6": "2606:4700:110:8a0e::1"}},
    "services": {"http_proxy": "172.16.0.1:2480"}
  }
}`

type seen struct {
	mu       sync.Mutex
	requests []string
	regBody  regRequest
	patchAut string
}

func fakeService(t *testing.T, status int) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/reg":
			_ = json.Unmarshal(raw, &s.regBody)
			w.WriteHeader(status)
			if status == http.StatusOK {
				_, _ = io.WriteString(w, sampleAnswer)
			} else {
				_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":1015}]}`)
			}
		case r.Method == http.MethodPatch:
			s.patchAut = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

func TestRegisterMakesAnAccountThatCanBringATunnelUp(t *testing.T) {
	srv, s := fakeService(t, http.StatusOK)
	c := &Client{API: srv.URL + "/v0", Now: func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }}

	a, err := c.Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "t.11111111-2222-3333-4444-555555555555" || a.Token != "tok-123" {
		t.Errorf("id/token = %q/%q", a.ID, a.Token)
	}
	if a.Endpoint != "162.159.192.7:2408" {
		t.Errorf("endpoint = %q, want the address with the name's port", a.Endpoint)
	}
	if a.Address != "172.16.0.2" {
		t.Errorf("address = %q", a.Address)
	}
	if a.PeerKey.Base64() != "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=" {
		t.Errorf("peer key = %s", a.PeerKey.Base64())
	}
	// The key sent is the public half of the private key kept.
	pub, _ := awg.PublicFrom(a.PrivateKey)
	if s.regBody.Key != pub.Base64() {
		t.Errorf("registered key %q is not the public half of the kept private key", s.regBody.Key)
	}
	if s.regBody.TOS != "2026-11-18T12:00:00.000Z" {
		t.Errorf("tos = %q", s.regBody.TOS)
	}
	if s.patchAut != "Bearer tok-123" {
		t.Errorf("the device was switched on with %q", s.patchAut)
	}
	if err := a.Validate(); err != nil {
		t.Error(err)
	}
}

func TestARefusalIsAnErrorWithWhatTheServiceSaid(t *testing.T) {
	srv, _ := fakeService(t, http.StatusTooManyRequests)
	_, err := (&Client{API: srv.URL + "/v0"}).Register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "1015") {
		t.Errorf("err = %v", err)
	}
}

func TestEndpointPrefersTheNamesPortAndNeedsAnIPv4Address(t *testing.T) {
	for _, tc := range []struct {
		v4, host string
		ports    []int
		want     string
	}{
		{"162.159.192.1:0", "engage.cloudflareclient.com:2408", nil, "162.159.192.1:2408"},
		{"162.159.192.1", "", []int{500}, "162.159.192.1:500"},
		{"162.159.192.1:0", "", nil, "162.159.192.1:2408"},
	} {
		got, err := endpointOf(tc.v4, tc.host, tc.ports)
		if err != nil || got != tc.want {
			t.Errorf("endpointOf(%q, %q, %v) = %q, %v; want %q", tc.v4, tc.host, tc.ports, got, err, tc.want)
		}
	}
	if _, err := endpointOf("", "engage.cloudflareclient.com:2408", nil); err == nil {
		t.Error("an answer without an address was taken")
	}
}

func TestAnAccountSurvivesARestart(t *testing.T) {
	srv, _ := fakeService(t, http.StatusOK)
	a, err := (&Client{API: srv.URL + "/v0"}).Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "warp.json")
	if got, err := Load(path); got != nil || err != nil {
		t.Fatalf("a missing file gave %v, %v", got, err)
	}
	if err := Save(path, a); err != nil {
		t.Fatal(err)
	}
	b, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.PrivateKey != a.PrivateKey || b.PeerKey != a.PeerKey || b.Endpoint != a.Endpoint || b.Address != a.Address {
		t.Errorf("read back %+v, saved %+v", b, a)
	}
}

func TestTheTracePageSaysWhetherTheRequestCameThroughWARP(t *testing.T) {
	on := "fl=123\nh=1.1.1.1\nip=104.28.1.2\nts=1.2\nvisit_scheme=https\nloc=NL\nwarp=on\ngateway=off\n"
	off := strings.Replace(on, "warp=on", "warp=off", 1)
	if !ThroughWARP([]byte(on)) {
		t.Error("warp=on not seen")
	}
	if ThroughWARP([]byte(off)) || ThroughWARP([]byte("<html>blocked</html>")) {
		t.Error("taken for WARP")
	}
	if !ThroughWARP([]byte(strings.Replace(on, "warp=on", "warp=plus", 1))) {
		t.Error("warp=plus not seen")
	}
}
