package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"awgrotator/internal/cherry"
	"awgrotator/internal/vultr"
)

func TestParsePlace(t *testing.T) {
	for in, want := range map[string]Place{
		"cherry:ams": {"cherry", "ams"},
		"vultr:sto":  {"vultr", "sto"},
		"waw":        {"vultr", "waw"}, // how places were written before
	} {
		got, err := ParsePlace(in)
		if err != nil || got != want {
			t.Errorf("ParsePlace(%q) = %v, %v; want %v", in, got, err, want)
		}
		if in != "waw" && got.String() != in {
			t.Errorf("%v.String() = %q", got, got.String())
		}
	}
	for _, bad := range []string{"", ":ams", "cherry:", "cherry:a:b", "cherry:a b"} {
		if _, err := ParsePlace(bad); err == nil {
			t.Errorf("ParsePlace(%q) accepted", bad)
		}
	}
}

func TestOpenKnowsBothProvidersAndNeedsAKey(t *testing.T) {
	if got := strings.Join(Names(), ","); got != "cherry,vultr" {
		t.Errorf("Names() = %s", got)
	}
	if _, err := Open("hetzner", Settings{Key: "k"}); err == nil {
		t.Error("an unknown provider opened")
	}
	if _, err := Open("vultr", Settings{Image: "2625", Tag: "t"}); err == nil {
		t.Error("a provider opened without a key")
	}
	if _, err := Open("cherry", Settings{Key: "k", Plan: "p", Image: "i", Tag: "t"}); err == nil {
		t.Error("cherry opened without a project of its own")
	}
	if _, err := Open("vultr", Settings{Key: "k", Image: "debian", Tag: "t"}); err == nil {
		t.Error("vultr opened with an image that is not an OS id")
	}
}

// fakeCherry is the slice of the Cherry API the adapter uses: our project 77
// holds one server of ours and one of the operator's without our tag; project
// 1 is the operator's other project, with their own server 7000001.
type fakeCherry struct {
	mu      sync.Mutex
	servers map[int]map[string]any
	deleted []int
	created map[string]any
}

func newFakeCherry() *fakeCherry {
	return &fakeCherry{servers: map[int]map[string]any{
		10: {"id": 10, "status": "deployed", "project": map[string]any{"id": 77}, "region": map[string]any{"slug": "NL-Amsterdam"},
			"tags": map[string]any{"awg-rotator": "awg-ams-g2"}, "hostname": "awg-ams-g2",
			"ip_addresses": []any{map[string]any{"address": "203.0.113.10", "address_family": 4, "type": "primary-ip"}}},
		11: {"id": 11, "status": "deployed", "project": map[string]any{"id": 77}, "tags": map[string]any{}},
		7000001: {"id": 7000001, "status": "deployed", "project": map[string]any{"id": 1},
			"tags": map[string]any{"awg-rotator": "x"}},
	}}
}

func (f *fakeCherry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var id int
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/projects/77/servers":
		var list []any
		for _, s := range f.servers {
			if s["project"].(map[string]any)["id"] == 77 {
				list = append(list, s)
			}
		}
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPost && r.URL.Path == "/projects/77/servers":
		_ = json.NewDecoder(r.Body).Decode(&f.created)
		if f.created["region"] == "SE-Stockholm" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"message":"Plan is out of stock in this region"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":12,"status":"pending","project":{"id":77},"region":{"slug":"NL-Amsterdam"},"tags":{"awg-rotator":"x"}}`))
	case r.Method == http.MethodGet && scan(r.URL.Path, "/servers/%d", &id):
		s, ok := f.servers[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"message":"Server not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(s)
	case r.Method == http.MethodDelete && scan(r.URL.Path, "/servers/%d", &id):
		f.deleted = append(f.deleted, id)
		delete(f.servers, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusTeapot)
	}
}

func scan(path, format string, id *int) bool {
	prefix, _, _ := strings.Cut(format, "%d")
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	n := 0
	for _, c := range path[len(prefix):] {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	*id = n
	return n > 0
}

func cherryFor(t *testing.T, f *fakeCherry) Provider {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p, err := NewCherry(cherry.New("k").WithBase(srv.URL), Settings{
		Plan: "B2-1-1gb-20s-shared", Image: "debian_13_64bit", Tag: "awg-rotator", Project: 77,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCherrySeesOnlyTaggedServersInItsProject(t *testing.T) {
	p := cherryFor(t, newFakeCherry())
	got, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "10" {
		t.Fatalf("List = %+v, want only server 10", got)
	}
	s := got[0]
	if s.Provider != "cherry" || s.Location != "ams" || s.IP != "203.0.113.10" || !s.Ready {
		t.Errorf("server = %+v", s)
	}
}

func TestCherryNeverDeletesSomeoneElsesServer(t *testing.T) {
	f := newFakeCherry()
	p := cherryFor(t, f)
	ctx := context.Background()

	for _, id := range []string{"11", "7000001"} {
		if err := p.Delete(ctx, id); !errors.Is(err, ErrNotOurs) {
			t.Errorf("Delete(%s) = %v, want ErrNotOurs", id, err)
		}
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v", f.deleted)
	}

	if err := p.Delete(ctx, "10"); err != nil {
		t.Fatalf("Delete(ours) = %v", err)
	}
	if err := p.Delete(ctx, "10"); err != nil {
		t.Errorf("deleting a server already gone = %v, want success", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != 10 {
		t.Errorf("deleted %v", f.deleted)
	}
}

func TestCherryCreateTagsAndBills(t *testing.T) {
	f := newFakeCherry()
	p := cherryFor(t, f)
	s, err := p.Create(context.Background(), Spec{Location: "ams", Label: "awg-ams-g3", SSHKeyID: "9", Billing: Monthly})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "12" || s.Location != "ams" {
		t.Errorf("server = %+v", s)
	}
	if f.created["region"] != "NL-Amsterdam" || f.created["cycle"] != "monthly" || f.created["image"] != "debian_13_64bit" {
		t.Errorf("request = %v", f.created)
	}
	if tags, _ := f.created["tags"].(map[string]any); tags["awg-rotator"] != "awg-ams-g3" {
		t.Errorf("tags = %v", f.created["tags"])
	}

	if _, err := p.Create(context.Background(), Spec{Location: "sto", Billing: Monthly}); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("sold out = %v, want ErrNoCapacity", err)
	}
	if _, err := p.Create(context.Background(), Spec{Location: "waw"}); err == nil {
		t.Error("a city Cherry does not have was accepted")
	}
}

func TestVultrRefusesUntaggedAndMonthly(t *testing.T) {
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/instances/mine":
			_, _ = w.Write([]byte(`{"instance":{"id":"mine","tags":["someone"],"region":"sto"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/instances/gone":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		case r.Method == http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	p, err := NewVultr(vultr.New("k").WithBase(srv.URL), Settings{Image: "2625", Tag: "awg-rotator", Plan: "vc2-1c-1gb"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := p.Delete(ctx, "mine"); !errors.Is(err, ErrNotOurs) {
		t.Errorf("Delete(untagged) = %v", err)
	}
	if err := p.Delete(ctx, "gone"); err != nil {
		t.Errorf("Delete(gone) = %v", err)
	}
	if deleted {
		t.Error("something was deleted")
	}
	if _, err := p.Create(ctx, Spec{Location: "sto", Billing: Monthly}); err == nil {
		t.Error("vultr accepted monthly billing")
	}
}

type countingProvider struct {
	Provider
	name string
	n    int
	err  error
}

func (c countingProvider) Name() string { return c.name }
func (c countingProvider) List(context.Context) ([]Server, error) {
	out := make([]Server, c.n)
	for i := range out {
		out[i].Provider = c.name
	}
	return out, c.err
}

func TestSetCountsEveryProviderAndFailsIfOneIsSilent(t *testing.T) {
	set := Set{"vultr": countingProvider{name: "vultr", n: 2}, "cherry": countingProvider{name: "cherry", n: 1}}
	all, err := set.List(context.Background())
	if err != nil || len(all) != 3 {
		t.Fatalf("List = %d, %v", len(all), err)
	}
	set["cherry"] = countingProvider{name: "cherry", err: errors.New("down")}
	if _, err := set.List(context.Background()); err == nil {
		t.Error("a provider that could not be asked was left out of the count")
	}
}

type slowProvider struct {
	Provider
	calls int
}

func (s *slowProvider) Name() string { return "slow" }
func (s *slowProvider) Get(context.Context, string) (*Server, error) {
	s.calls++
	return &Server{Ready: s.calls >= 3}, nil
}

func TestWaitReadyPollsUntilReady(t *testing.T) {
	p := &slowProvider{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := waitReady(ctx, p, "x", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if p.calls != 3 {
		t.Errorf("calls = %d", p.calls)
	}
}
