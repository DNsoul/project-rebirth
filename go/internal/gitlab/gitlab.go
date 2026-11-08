// Package gitlab is the small slice of GitLab this project needs to keep a second
// copy of what it publishes: its generic package registry. The installers go
// there, and so do the small signed files that are also in the gist.
//
// A copy needs no trust. What makes an installer the right one is the size and
// SHA-256 in the signed announcement, and an app refuses a file that does not
// match, whoever served it; the signed files are verified the same way wherever
// they are found. So the copies are kept in a public project and read without a
// token; the token is only for putting files there.
package gitlab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultBase is GitLab's own address.
const DefaultBase = "https://gitlab.com"

// DefaultPackage is the name of the package the installers are kept under.
const DefaultPackage = "rebirth"

// DefaultSyncPackage is the package the small signed files of the configuration
// (the same ones that live in the gist) are kept under, and SyncVersion the one
// version they all share: a file is replaced by uploading it again.
const (
	DefaultSyncPackage = "sync"
	SyncVersion        = "1"
)

// Client talks to a GitLab with a token that may write packages.
type Client struct {
	Base  string
	Token string

	// Deploy says the token is a deploy token, which GitLab wants in a header of
	// its own. A deploy token reaches nothing but the project's package registry,
	// which is why it is the one a server may hold.
	Deploy bool

	HTTP *http.Client
}

// New returns a client for the GitLab at base (DefaultBase when empty).
func New(base, token string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{
		Base:  strings.TrimRight(base, "/"),
		Token: token,
		// Files are tens of megabytes; the timeout is for the whole exchange.
		HTTP: &http.Client{Timeout: 15 * time.Minute},
	}
}

// ResolveProject returns the numeric id of the project at path ("owner/name").
// A public project can be looked up without a token.
func (c *Client) ResolveProject(ctx context.Context, path string) (int64, error) {
	if strings.Count(path, "/") < 1 || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return 0, fmt.Errorf("путь проекта %q должен быть вида владелец/имя", path)
	}
	endpoint := c.Base + "/api/v4/projects/" + url.PathEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return 0, fmt.Errorf("проект %s не найден (он должен существовать и быть публичным, либо нужен токен с доступом)", path)
	default:
		return 0, fmt.Errorf("gitlab GET %s: %s: %s", endpoint, resp.Status, firstLine(raw))
	}
	var out struct {
		ID         int64  `json:"id"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == 0 {
		return 0, fmt.Errorf("gitlab ответил на %s не так, как ожидалось", endpoint)
	}
	if out.Visibility != "" && out.Visibility != "public" {
		return 0, fmt.Errorf("проект %s не публичный (%s): приложения не смогут скачивать из него без токена", path, out.Visibility)
	}
	return out.ID, nil
}

// PackageURL is the address a package file can be downloaded from, which for a
// public project needs no token.
func PackageURL(base string, project int64, pkg, version, file string) string {
	if base == "" {
		base = DefaultBase
	}
	return fmt.Sprintf("%s/api/v4/projects/%d/packages/generic/%s/%s/%s",
		strings.TrimRight(base, "/"), project, url.PathEscape(pkg), url.PathEscape(version), url.PathEscape(file))
}

// PackageTemplate is the same address with {version} and {file} left in, as an
// app's build carries it.
func PackageTemplate(base string, project int64, pkg string) string {
	if base == "" {
		base = DefaultBase
	}
	return fmt.Sprintf("%s/api/v4/projects/%d/packages/generic/%s/{version}/{file}",
		strings.TrimRight(base, "/"), project, url.PathEscape(pkg))
}

// SyncTemplate is the address of one of the synced files, with {file} left in
// for the app to fill.
func SyncTemplate(base string, project int64, pkg string) string {
	if base == "" {
		base = DefaultBase
	}
	return fmt.Sprintf("%s/api/v4/projects/%d/packages/generic/%s/%s/{file}",
		strings.TrimRight(base, "/"), project, url.PathEscape(pkg), SyncVersion)
}

// UploadFile puts the file at path into the project's generic package registry
// under the given name and returns the address it can be downloaded from. The
// name is the one an app will ask for: the last part of the announced address.
func (c *Client) UploadFile(ctx context.Context, project int64, pkg, version, name, path string) (string, error) {
	if c.Token == "" {
		return "", errors.New("нужен токен GitLab (GITLAB_TOKEN в secrets.env) с правом api")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if name == "" {
		name = info.Name()
	}
	addr := PackageURL(c.Base, project, pkg, version, name)
	if err := c.put(ctx, addr, f, info.Size()); err != nil {
		return "", err
	}
	return addr, nil
}

// PutFile stores a small file in the registry under the given name, replacing
// what was there: the newest upload is what a download returns.
func (c *Client) PutFile(ctx context.Context, project int64, pkg, version, name string, content []byte) error {
	if c.Token == "" {
		return errors.New("нет токена GitLab: записать нечего и некуда")
	}
	return c.put(ctx, PackageURL(c.Base, project, pkg, version, name), bytes.NewReader(content), int64(len(content)))
}

func (c *Client) put(ctx context.Context, addr string, body io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, addr, body)
	if err != nil {
		return err
	}
	// A bare reader does not tell the request how long it is.
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gitlab PUT %s: %s: %s", addr, resp.Status, firstLine(raw))
	}
	return nil
}

// GetFile reads a small file from the registry the way an app does, without a
// token. A file that is not there is not an error: found is false.
func (c *Client) GetFile(ctx context.Context, project int64, pkg, version, name string) (content []byte, found bool, err error) {
	addr := PackageURL(c.Base, project, pkg, version, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return raw, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("gitlab GET %s: %s: %s", addr, resp.Status, firstLine(raw))
}

// CreateDeployToken makes a deploy token for the project that can read and
// write its package registry and nothing else, and returns it. GitLab shows the
// value once, so the caller has to keep it. It needs a token of the operator's
// own with the api scope.
func (c *Client) CreateDeployToken(ctx context.Context, project int64, name string) (string, error) {
	if c.Token == "" || c.Deploy {
		return "", errors.New("чтобы выпустить токен для серверов, нужен GITLAB_TOKEN с правом api")
	}
	body, err := json.Marshal(map[string]any{
		"name":   name,
		"scopes": []string{"read_package_registry", "write_package_registry"},
	})
	if err != nil {
		return "", err
	}
	addr := fmt.Sprintf("%s/api/v4/projects/%d/deploy_tokens", c.Base, project)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gitlab POST %s: %s: %s", addr, resp.Status, firstLine(raw))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("gitlab ответил на %s без токена", addr)
	}
	return out.Token, nil
}

// CheckDownload fetches the address the way an app does, without a token, and
// confirms it is the file the announcement will name. It is what tells the
// operator that a copy is really there before anyone is sent to it.
func CheckDownload(ctx context.Context, client *http.Client, addr, wantSHA256 string, wantSize uint64) error {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("без токена %s отвечает %s: проект публичный, а реестр пакетов открыт для всех?", addr, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		return err
	}
	if uint64(n) != wantSize {
		return fmt.Errorf("по %s отдаётся %d байт вместо %d", addr, n, wantSize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA256 {
		return fmt.Errorf("по %s отдаётся файл с другой контрольной суммой (%s…)", addr, got[:16])
	}
	return nil
}

func (c *Client) authorize(req *http.Request) {
	switch {
	case c.Token == "":
	case c.Deploy:
		req.Header.Set("DEPLOY-TOKEN", c.Token)
	default:
		req.Header.Set("PRIVATE-TOKEN", c.Token)
	}
}

func firstLine(b []byte) string {
	var msg struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(b, &msg) == nil {
		if msg.Error != "" {
			return msg.Error
		}
		if msg.Message != nil {
			return fmt.Sprint(msg.Message)
		}
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
