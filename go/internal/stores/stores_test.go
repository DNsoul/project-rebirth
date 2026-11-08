package stores

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"awgrotator/internal/proto"
)

var t0 = time.Unix(1_800_000_000, 0)

// fakeStore is a place that lives in a map and can be made to fail.
type fakeStore struct {
	name string

	mu       sync.Mutex
	files    map[string]string
	readErr  error
	writeErr error
	writes   []map[string]string
}

func newStore(name string) *fakeStore { return &fakeStore{name: name, files: map[string]string{}} }

func (f *fakeStore) Name() string { return f.name }

func (f *fakeStore) Read(_ context.Context, names ...string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := map[string]string{}
	for _, n := range names {
		if v, ok := f.files[n]; ok {
			out[n] = v
		}
	}
	return out, nil
}

func (f *fakeStore) Write(_ context.Context, files map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	copied := map[string]string{}
	for k, v := range files {
		f.files[k] = v
		copied[k] = v
	}
	f.writes = append(f.writes, copied)
	return nil
}

// world is the operator's keys and a way to make signed files with them.
type world struct {
	t       *testing.T
	root    ed25519.PrivateKey
	server  ed25519.PrivateKey
	content []byte
	cert    proto.Cert
	trust   proto.Trust
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root, err := proto.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	backup, err := proto.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	server, err := proto.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	content, err := proto.GenerateContentKey()
	if err != nil {
		t.Fatal(err)
	}
	cert := proto.Cert{
		Epoch:     1,
		NotBefore: t0.Add(-time.Hour).Unix(),
		NotAfter:  t0.Add(365 * 24 * time.Hour).Unix(),
		ServerPub: proto.Encode(proto.PublicOf(server)),
	}
	if err := cert.Sign(proto.IssuerRoot, root); err != nil {
		t.Fatal(err)
	}
	return &world{
		t: t, root: root, server: server, content: content, cert: cert,
		trust: proto.Trust{
			RootPub:    proto.Encode(proto.PublicOf(root)),
			BackupPub:  proto.Encode(proto.PublicOf(backup)),
			ContentKey: proto.Encode(content),
		},
	}
}

func (w *world) doc(kind string, seq uint64, issued time.Time) string {
	w.t.Helper()
	blob, err := proto.SealDoc(&proto.Doc{
		Kind: kind, Seq: seq, IssuedAt: issued.Unix(), ExpiresAt: issued.Add(60 * 24 * time.Hour).Unix(),
		Cert: w.cert, Body: `{"n":` + string(rune('0'+seq%10)) + `}`,
	}, w.server, w.content)
	if err != nil {
		w.t.Fatal(err)
	}
	return blob
}

func (w *world) config(seq uint64) string { return w.doc(proto.KindConfig, seq, t0) }

func (w *world) revocation(epoch uint32) string {
	w.t.Helper()
	blob, err := proto.SealRevocation(&proto.Revocation{MinEpoch: epoch, IssuedAt: t0.Unix()}, proto.IssuerRoot, w.root, w.content)
	if err != nil {
		w.t.Fatal(err)
	}
	return blob
}

func (w *world) release(build uint32) string {
	w.t.Helper()
	blob, err := proto.SealRelease(&proto.Release{
		Version: "1.2.3", Build: build, IssuedAt: t0.Unix(),
		Assets: []proto.ReleaseAsset{{OS: proto.OSWindows, URL: "https://example.com/a.exe", SHA256: strings.Repeat("a", 64), Size: 10}},
	}, proto.IssuerRoot, w.root, w.content)
	if err != nil {
		w.t.Fatal(err)
	}
	return blob
}

func (w *world) command(issued time.Time) string {
	w.t.Helper()
	id, err := proto.NewCommandID()
	if err != nil {
		w.t.Fatal(err)
	}
	blob, err := proto.SealCommand(&proto.Command{ID: id, IssuedAt: issued.Unix(), Action: proto.ActionStatus}, proto.IssuerRoot, w.root, w.content)
	if err != nil {
		w.t.Fatal(err)
	}
	return blob
}

func (w *world) placeholder() string {
	w.t.Helper()
	blob, err := proto.Seal(w.content, []byte("{}"))
	if err != nil {
		w.t.Fatal(err)
	}
	return blob
}

func (w *world) set(stores ...Store) *Set {
	return &Set{Stores: stores, Trust: w.trust, Now: func() time.Time { return t0 }}
}

func TestReadTakesTheNewestOfEachFileFromEitherPlace(t *testing.T) {
	w := newWorld(t)
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	github.files[proto.FileConfig] = w.config(5)
	gitlab.files[proto.FileConfig] = w.config(7) // GitLab is ahead for the configuration…
	github.files[proto.FileRelease] = w.release(9)
	gitlab.files[proto.FileRelease] = w.release(8) // …GitHub for the announcement
	gitlab.files[proto.FileRevocation] = w.revocation(3)

	got, err := w.set(github, gitlab).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[proto.FileConfig] != gitlab.files[proto.FileConfig] {
		t.Error("the newer configuration, from GitLab, was not chosen")
	}
	if got[proto.FileRelease] != github.files[proto.FileRelease] {
		t.Error("the newer announcement, from GitHub, was not chosen")
	}
	if got[proto.FileRevocation] != gitlab.files[proto.FileRevocation] {
		t.Error("a file only one place has was dropped")
	}
	if _, ok := got[proto.FileCommand]; ok {
		t.Error("a file no place has appeared from nowhere")
	}
}

func TestReadSurvivesOnePlaceBeingDown(t *testing.T) {
	w := newWorld(t)
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	github.readErr = errors.New("503")
	gitlab.files[proto.FileConfig] = w.config(2)

	got, err := w.set(github, gitlab).Read(context.Background(), proto.FileConfig)
	if err != nil || got[proto.FileConfig] != gitlab.files[proto.FileConfig] {
		t.Fatalf("got %v, err %v", got, err)
	}

	gitlab.readErr = errors.New("timeout")
	_, err = w.set(github, gitlab).Read(context.Background(), proto.FileConfig)
	if err == nil || !strings.Contains(err.Error(), "GitHub") || !strings.Contains(err.Error(), "GitLab") {
		t.Errorf("with both down the error should name both: %v", err)
	}
}

func TestAFileThatDoesNotVerifyNeverBeatsOneThatDoes(t *testing.T) {
	w := newWorld(t)
	other := newWorld(t) // somebody else's keys: signed, but not by us
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	github.files[proto.FileConfig] = other.config(99)
	gitlab.files[proto.FileConfig] = w.config(1)

	got, err := w.set(github, gitlab).Read(context.Background(), proto.FileConfig)
	if err != nil || got[proto.FileConfig] != gitlab.files[proto.FileConfig] {
		t.Fatalf("a forgery with a big number won: err %v", err)
	}

	// A placeholder is not newer than anything either.
	github.files[proto.FileConfig] = w.placeholder()
	got, _ = w.set(github, gitlab).Read(context.Background(), proto.FileConfig)
	if got[proto.FileConfig] != gitlab.files[proto.FileConfig] {
		t.Error("a placeholder beat a real configuration")
	}

	// With nothing real anywhere, what exists is still returned, so that a tool
	// can show it.
	gitlab.files[proto.FileConfig] = w.placeholder()
	got, _ = w.set(github, gitlab).Read(context.Background(), proto.FileConfig)
	if got[proto.FileConfig] == "" {
		t.Error("the placeholder was hidden")
	}
}

func TestAnExpiredFileLosesToALiveOne(t *testing.T) {
	w := newWorld(t)
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	github.files[proto.FileConfig] = w.doc(proto.KindConfig, 50, t0.Add(-100*24*time.Hour)) // long expired
	gitlab.files[proto.FileConfig] = w.config(3)

	got, _ := w.set(github, gitlab).Read(context.Background(), proto.FileConfig)
	if got[proto.FileConfig] != gitlab.files[proto.FileConfig] {
		t.Error("an expired document won because of its number")
	}
}

func TestWriteGoesEverywhereAndSucceedsWithOne(t *testing.T) {
	w := newWorld(t)
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	set := w.set(github, gitlab)

	res, err := set.Write(context.Background(), map[string]string{proto.FileConfig: "x"})
	if err != nil || res.Accepted() != 2 || len(res.Failed()) != 0 {
		t.Fatalf("both places up: %+v, %v", res, err)
	}

	gitlab.writeErr = errors.New("502 bad gateway")
	res, err = set.Write(context.Background(), map[string]string{proto.FileConfig: "y"})
	if err != nil {
		t.Fatalf("one place down must not fail the write: %v", err)
	}
	if res.Accepted() != 1 || len(res.Failed()) != 1 || res.Failed()[0].Store != "GitLab" ||
		!strings.Contains(res.String(), "502") {
		t.Errorf("the failure should be reported: %+v", res)
	}
	if github.files[proto.FileConfig] != "y" || gitlab.files[proto.FileConfig] != "x" {
		t.Error("each place should hold what it accepted")
	}

	github.writeErr = errors.New("403")
	if _, err := set.Write(context.Background(), map[string]string{proto.FileConfig: "z"}); err == nil {
		t.Error("a write nobody took was reported as done")
	}
}

func TestReconcileCatchesUpWhicheverPlaceIsBehind(t *testing.T) {
	w := newWorld(t)
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	github.files[proto.FileConfig] = w.config(5)
	gitlab.files[proto.FileConfig] = w.config(6)
	github.files[proto.FileRelease] = w.release(9)
	// GitLab has no announcement, and a placeholder where GitHub has a command.
	github.files[proto.FileCommand] = w.command(t0)
	gitlab.files[proto.FileCommand] = w.placeholder()

	rep, err := w.set(github, gitlab).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Synced() {
		t.Fatalf("not synced: %s", rep)
	}
	if github.files[proto.FileConfig] != gitlab.files[proto.FileConfig] {
		t.Error("GitHub did not catch up with GitLab's configuration")
	}
	if gitlab.files[proto.FileRelease] != github.files[proto.FileRelease] {
		t.Error("GitLab did not get the announcement")
	}
	if gitlab.files[proto.FileCommand] != github.files[proto.FileCommand] {
		t.Error("a placeholder was not replaced by the real command")
	}
	if got := strings.Join(rep.Copied["GitHub"], ","); got != proto.FileConfig {
		t.Errorf("GitHub was given %q", got)
	}
	if got := strings.Join(rep.Copied["GitLab"], ","); got != proto.FileCommand+","+proto.FileRelease {
		t.Errorf("GitLab was given %q", got)
	}

	// Once they agree nothing is written at all.
	before := len(github.writes) + len(gitlab.writes)
	rep, _ = w.set(github, gitlab).Reconcile(context.Background())
	if !rep.Synced() || len(github.writes)+len(gitlab.writes) != before || rep.String() != "all places agree" {
		t.Errorf("agreeing places were written to again: %s", rep)
	}
}

func TestReconcileNeverWritesAnOlderVersionOverANewerOne(t *testing.T) {
	// A server that has been replaced still wakes up now and then. What it
	// finds in the places is its successor's newer configuration, and that is
	// what it must spread, not anything of its own.
	w := newWorld(t)
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	github.files[proto.FileConfig] = w.config(10)
	gitlab.files[proto.FileConfig] = w.config(9)

	if _, err := w.set(github, gitlab).Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(github.writes) != 0 {
		t.Error("the newer place was written to")
	}
	if gitlab.files[proto.FileConfig] != github.files[proto.FileConfig] {
		t.Error("the older place was not brought up")
	}
}

func TestReconcileReportsAPlaceItCannotReachOrWriteAndTriesAgain(t *testing.T) {
	w := newWorld(t)
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	github.files[proto.FileConfig] = w.config(4)
	set := w.set(github, gitlab)

	gitlab.writeErr = errors.New("rate limited")
	rep, err := set.Reconcile(context.Background(), proto.FileConfig)
	if err != nil || rep.Synced() || rep.Failed["GitLab"] == nil || !strings.Contains(rep.String(), "still behind") {
		t.Fatalf("a failed catch-up should show: %s (%v)", rep, err)
	}

	gitlab.writeErr = nil
	gitlab.readErr = errors.New("unreachable")
	rep, err = set.Reconcile(context.Background(), proto.FileConfig)
	if err != nil || rep.Synced() || rep.Missed["GitLab"] == nil {
		t.Fatalf("an unreachable place should show: %s (%v)", rep, err)
	}

	gitlab.readErr = nil
	rep, err = set.Reconcile(context.Background(), proto.FileConfig)
	if err != nil || !rep.Synced() || gitlab.files[proto.FileConfig] != github.files[proto.FileConfig] {
		t.Fatalf("it should catch up the next time: %s (%v)", rep, err)
	}

	github.readErr, gitlab.readErr = errors.New("a"), errors.New("b")
	if _, err := set.Reconcile(context.Background()); err == nil {
		t.Error("with nothing readable it should say so")
	}
}

func TestReconcileDoesNotSpreadWhatDoesNotVerify(t *testing.T) {
	w := newWorld(t)
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	github.files[proto.FileConfig] = newWorld(t).config(99) // not ours
	rep, err := w.set(github, gitlab).Reconcile(context.Background())
	if err != nil || len(rep.Copied) != 0 || gitlab.files[proto.FileConfig] != "" {
		t.Errorf("a forgery was copied: %s (%v)", rep, err)
	}
}

func TestRevocationsAreRankedByEpochAndCommandsByTime(t *testing.T) {
	w := newWorld(t)
	github, gitlab := newStore("GitHub"), newStore("GitLab")
	github.files[proto.FileRevocation] = w.revocation(2)
	gitlab.files[proto.FileRevocation] = w.revocation(5)
	github.files[proto.FileCommand] = w.command(t0.Add(-time.Minute))
	gitlab.files[proto.FileCommand] = w.command(t0)

	got, _ := w.set(github, gitlab).Read(context.Background(), proto.FileRevocation, proto.FileCommand)
	if got[proto.FileRevocation] != gitlab.files[proto.FileRevocation] {
		t.Error("the higher revocation epoch should win")
	}
	if got[proto.FileCommand] != gitlab.files[proto.FileCommand] {
		t.Error("the later command should win")
	}
}
