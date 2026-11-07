package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecrets(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, SecretsFile), []byte(body), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
}

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	writeSecrets(t, dir, `# комментарий
VULTR_API_KEY=plain

  GITHUB_TOKEN = "quoted value"
SINGLE='also quoted'
broken line without equals
`)
	got, err := parseEnvFile(filepath.Join(dir, SecretsFile))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{
		VultrKeyName:  "plain",
		GitHubKeyName: "quoted value",
		"SINGLE":      "also quoted",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d keys, want %d: %v", len(got), len(want), got)
	}
}

func TestParseEnvFileMissingIsEmpty(t *testing.T) {
	got, err := parseEnvFile(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestLoadSecretsNamesWhatIsMissing(t *testing.T) {
	t.Setenv(VultrKeyName, "")
	t.Setenv(GitHubKeyName, "")
	dir := t.TempDir()
	writeSecrets(t, dir, VultrKeyName+"=only-this\n")

	_, err := LoadSecrets(dir)
	if err == nil {
		t.Fatal("missing token was accepted")
	}
	if !strings.Contains(err.Error(), GitHubKeyName) {
		t.Errorf("error does not name the missing key: %v", err)
	}
	if strings.Contains(err.Error(), VultrKeyName) {
		t.Errorf("error names a key that was present: %v", err)
	}
}

func TestEnvironmentOverridesFile(t *testing.T) {
	dir := t.TempDir()
	writeSecrets(t, dir, VultrKeyName+"=from-file\n"+GitHubKeyName+"=from-file\n")
	t.Setenv(VultrKeyName, "from-env")
	t.Setenv(GitHubKeyName, "")

	got, err := LoadSecrets(dir)
	if err != nil {
		t.Fatalf("LoadSecrets: %v", err)
	}
	if got.VultrAPIKey != "from-env" {
		t.Errorf("vultr key = %q, want the environment value", got.VultrAPIKey)
	}
	if got.GitHubToken != "from-file" {
		t.Errorf("github token = %q, want the file value", got.GitHubToken)
	}
}

func TestRootHonoursEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWGCTL_DIR", dir)
	got, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	want, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Root = %q, want %q", got, want)
	}
}

func TestRootFindsProjectFromSubdirectory(t *testing.T) {
	t.Setenv("AWGCTL_DIR", "")
	root := t.TempDir()
	writeSecrets(t, root, "")
	sub := filepath.Join(root, "go", "cmd", "awgctl")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	got, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	// Compare resolved paths: temp dirs can be reached through symlinks.
	gotResolved, _ := filepath.EvalSymlinks(got)
	wantResolved, _ := filepath.EvalSymlinks(root)
	if gotResolved != wantResolved {
		t.Errorf("Root = %q, want %q", gotResolved, wantResolved)
	}
}

func TestRegionsAreConfigured(t *testing.T) {
	if len(Regions) == 0 {
		t.Fatal("no regions configured")
	}
	seen := map[string]bool{}
	for _, r := range Regions {
		if seen[r] {
			t.Errorf("region %q listed twice", r)
		}
		seen[r] = true
	}
}
