// Package gist is the small slice of the GitHub API this project needs.
//
// The gist is "secret", which on GitHub means unlisted rather than access
// controlled: anyone who learns the id can read it without a token. That is
// why everything stored there goes through the envelope in package proto, and
// why the apps never need the GitHub token to read.
package gist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const api = "https://api.github.com"

// Client talks to the GitHub gist API with a fine-grained token that has no
// permission beyond gists.
type Client struct {
	token string
	base  string
	http  *http.Client
}

// New returns a client. An empty token still works for reading a secret gist.
func New(token string) *Client {
	return &Client{
		token: token,
		base:  api,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

type gistFile struct {
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	RawURL    string `json:"raw_url,omitempty"`
}

type gistBody struct {
	ID          string              `json:"id,omitempty"`
	Description string              `json:"description"`
	Public      bool                `json:"public"`
	Files       map[string]gistFile `json:"files"`
	Owner       struct {
		Login string `json:"login"`
	} `json:"owner,omitempty"`
}

// Create makes a new secret gist holding the given files and returns its id
// and owner.
func (c *Client) Create(ctx context.Context, files map[string]string) (id, owner string, err error) {
	body := gistBody{
		// Left empty on purpose: a description is visible to anyone with
		// the link.
		Description: "",
		Public:      false,
		Files:       toFiles(files),
	}
	var out gistBody
	if err := c.do(ctx, http.MethodPost, c.base+"/gists", body, &out); err != nil {
		return "", "", err
	}
	return out.ID, out.Owner.Login, nil
}

// Update replaces the content of the named files. Files not named are left
// alone.
func (c *Client) Update(ctx context.Context, id string, files map[string]string) error {
	body := struct {
		Files map[string]gistFile `json:"files"`
	}{Files: toFiles(files)}
	return c.do(ctx, http.MethodPatch, c.base+"/gists/"+id, body, nil)
}

// Get reads every file in the gist.
func (c *Client) Get(ctx context.Context, id string) (map[string]string, error) {
	var out gistBody
	if err := c.do(ctx, http.MethodGet, c.base+"/gists/"+id, nil, &out); err != nil {
		return nil, err
	}
	files := make(map[string]string, len(out.Files))
	for name, f := range out.Files {
		if f.Truncated {
			// Our payloads are small; if this ever happens the file is not
			// ours or something is very wrong.
			return nil, fmt.Errorf("gist file %q came back truncated", name)
		}
		files[name] = f.Content
	}
	return files, nil
}

// Delete removes the gist.
func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, c.base+"/gists/"+id, nil, nil)
}

func toFiles(files map[string]string) map[string]gistFile {
	out := make(map[string]gistFile, len(files))
	for name, content := range files {
		out[name] = gistFile{Content: content}
	}
	return out
}

func (c *Client) do(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github %s %s: %s: %s", method, url, resp.Status, firstLine(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("github response is not valid JSON: %w", err)
		}
	}
	return nil
}

func firstLine(b []byte) string {
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &msg) == nil && msg.Message != "" {
		return msg.Message
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
