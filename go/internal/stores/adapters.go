package stores

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"awgrotator/internal/gist"
	"awgrotator/internal/gitlab"
)

// How long one read or write against a place may take, so that one that hangs
// cannot hold up the others for long. A server looks at its inbox every half a
// minute, so a read is given less time than a write, which may be several files.
const (
	readTimeout  = 20 * time.Second
	writeTimeout = 45 * time.Second
)

// Gist is the secret gist on GitHub.
type Gist struct {
	Client *gist.Client
	ID     string
}

// Name implements Store.
func (g Gist) Name() string { return "GitHub" }

// Read implements Store.
func (g Gist) Read(ctx context.Context, names ...string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	files, err := g.Client.Get(ctx, g.ID)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return files, nil
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		if blob, ok := files[name]; ok {
			out[name] = blob
		}
	}
	return out, nil
}

// Write implements Store.
func (g Gist) Write(ctx context.Context, files map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return g.Client.Update(ctx, g.ID, files)
}

// GitLab is a package in a public GitLab project. The files are read without a
// token, as the apps read them; the token is only for writing.
type GitLab struct {
	Client  *gitlab.Client
	Project int64
	Package string
}

// Name implements Store.
func (g GitLab) Name() string { return "GitLab" }

// Read implements Store.
func (g GitLab) Read(ctx context.Context, names ...string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	if len(names) == 0 {
		names = Names
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		content, found, err := g.Client.GetFile(ctx, g.Project, g.Package, gitlab.SyncVersion, name)
		if err != nil {
			return nil, err
		}
		if found {
			out[name] = strings.TrimSpace(string(content))
		}
	}
	return out, nil
}

// Write implements Store. Every file is tried even if one fails, so that a
// single bad moment does not leave the rest behind as well.
func (g GitLab) Write(ctx context.Context, files map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []error
	for _, name := range names {
		if err := g.Client.PutFile(ctx, g.Project, g.Package, gitlab.SyncVersion, name, []byte(files[name])); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
