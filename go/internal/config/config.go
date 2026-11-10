// Package config finds the project root and reads secrets.env.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Names of the files and keys this project uses.
const (
	SecretsFile   = "secrets.env"
	VultrKeyName  = "VULTR_API_KEY"
	GitHubKeyName = "GITHUB_TOKEN"
	GitLabKeyName = "GITLAB_TOKEN"

	// TelegramKeyName is the operator's bot. Optional: without it there is no
	// bot, and devices cannot be made to wait for approval.
	TelegramKeyName = "TELEGRAM_BOT_TOKEN"
)

// Secrets are the credentials the operator supplies. The first two are needed
// for everything; the GitLab token only for keeping a second copy of the
// installers there.
type Secrets struct {
	VultrAPIKey string
	GitHubToken string
	GitLabToken string // optional: needed only to upload to a GitLab mirror

	TelegramToken string // optional: the operator's bot
}

// Root returns the project root. It honours AWGCTL_DIR, otherwise it walks up
// from the working directory and then from the directory holding the binary,
// so the tool works both from inside the project and from anywhere else.
func Root() (string, error) {
	if dir := os.Getenv("AWGCTL_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if root, ok := findRoot(wd); ok {
		return root, nil
	}
	// The binary normally sits in dist/ inside the project, so looking up
	// from there covers being run by its full path from somewhere else.
	if exe, err := os.Executable(); err == nil {
		if root, ok := findRoot(filepath.Dir(exe)); ok {
			return root, nil
		}
	}
	// Nothing found; fall back to the working directory so a fresh checkout
	// can still be initialised.
	return wd, nil
}

// findRoot walks up from start looking for something that marks the project
// root.
func findRoot(start string) (string, bool) {
	dir := start
	for {
		for _, marker := range []string{SecretsFile, "state", "spec"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// LoadSecrets reads secrets.env. Environment variables of the same name win,
// which is handy for one-off overrides.
func LoadSecrets(root string) (Secrets, error) {
	values, err := parseEnvFile(filepath.Join(root, SecretsFile))
	if err != nil {
		return Secrets{}, err
	}
	s := Secrets{
		VultrAPIKey: pick(os.Getenv(VultrKeyName), values[VultrKeyName]),
		GitHubToken: pick(os.Getenv(GitHubKeyName), values[GitHubKeyName]),
		GitLabToken: pick(os.Getenv(GitLabKeyName), values[GitLabKeyName]),

		TelegramToken: pick(os.Getenv(TelegramKeyName), values[TelegramKeyName]),
	}
	var missing []string
	if s.VultrAPIKey == "" {
		missing = append(missing, VultrKeyName)
	}
	if s.GitHubToken == "" {
		missing = append(missing, GitHubKeyName)
	}
	if len(missing) > 0 {
		return s, fmt.Errorf("%s is missing %s", filepath.Join(root, SecretsFile), strings.Join(missing, " and "))
	}
	return s, nil
}

func pick(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	values := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Allow quoted values.
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		values[key] = val
	}
	return values, sc.Err()
}
