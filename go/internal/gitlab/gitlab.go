// Package gitlab is the small slice of GitLab this project needs to keep a second
// copy of the installers: its generic package registry.
//
// A copy needs no trust. What makes an installer the right one is the size and
// SHA-256 in the signed announcement, and an app refuses a file that does not
// match, whoever served it. So the copies are kept in a public project and read
// without a token; the token is only for putting files there.
package gitlab

import (
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

// Client talks to a GitLab with a token that may write packages.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, addr, f)
	if err != nil {
		return "", err
	}
	// An *os.File does not tell the request how long it is.
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gitlab PUT %s: %s: %s", addr, resp.Status, firstLine(raw))
	}
	return addr, nil
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
	if c.Token != "" {
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
