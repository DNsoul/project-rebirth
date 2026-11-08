package main

import (
	"encoding/json"
	"strings"
	"testing"

	"awgrotator/internal/proto"
	"awgrotator/internal/store"
)

func gitLabMirror() store.Mirror {
	return store.Mirror{Kind: store.KindGitLab, Base: "https://gitlab.com", ProjectID: 5, Path: "a/b", Package: "rebirth"}
}

func TestBootstrapCarriesTheConfigMirrorsToo(t *testing.T) {
	trust := proto.Trust{RootPub: "r", BackupPub: "b", ContentKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}
	st := &store.State{GistID: "g", GistOwner: "o"}

	raw, err := json.Marshal(newBootstrapBundle(trust, st))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "config_mirrors") {
		t.Errorf("a build without a second place must not mention one: %s", raw)
	}

	st.SetMirror(gitLabMirror())
	raw, err = json.Marshal(newBootstrapBundle(trust, st))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ConfigMirrors []string `json:"config_mirrors"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := "https://gitlab.com/api/v4/projects/5/packages/generic/sync/1/{file}"
	if len(got.ConfigMirrors) != 1 || got.ConfigMirrors[0] != want {
		t.Errorf("config_mirrors = %v, want [%s]", got.ConfigMirrors, want)
	}
	if strings.Contains(string(raw), "deploy") {
		t.Errorf("a token must never reach an app build: %s", raw)
	}
}

func TestAServerIsGivenTheSecondPlaceOnlyOnceThereIsATokenForIt(t *testing.T) {
	st := &store.State{}
	if gitLabSyncFor(st) != nil {
		t.Error("no mirror, yet a second place was configured")
	}
	st.SetMirror(gitLabMirror())
	if gitLabSyncFor(st) != nil {
		t.Error("a mirror without a token for servers cannot be written to")
	}

	m := gitLabMirror()
	m.DeployToken = "gldt-secret"
	st.SetMirror(m)
	got := gitLabSyncFor(st)
	if got == nil || got.Project != 5 || got.Package != "sync" || got.Token != "gldt-secret" || got.Base != "https://gitlab.com" {
		t.Errorf("second place = %+v", got)
	}
}

func TestSettingTheMirrorAgainKeepsTheTokenForTheSameProject(t *testing.T) {
	st := &store.State{}
	m := gitLabMirror()
	m.DeployToken = "gldt-secret"
	m.SyncPackage = "mine"
	st.SetMirror(m)

	// Set up again, as `mirror set` does: the same project keeps what it had.
	st.SetMirror(gitLabMirror())
	got, _ := st.GitLabMirror()
	if got.DeployToken != "gldt-secret" || got.SyncPkg() != "mine" {
		t.Errorf("lost what was set up: %+v", got)
	}

	// Another project is another place: its token is not the old one.
	other := gitLabMirror()
	other.ProjectID = 6
	st.SetMirror(other)
	got, _ = st.GitLabMirror()
	if got.DeployToken != "" || got.SyncPkg() != "sync" {
		t.Errorf("a different project inherited a token: %+v", got)
	}
}
