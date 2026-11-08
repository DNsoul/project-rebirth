package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
)

// packageFile is one upload in a package.
type packageFile struct {
	ID       int64  `json:"id"`
	FileName string `json:"file_name"`
}

// PruneFiles deletes the older uploads of every file in a package version,
// keeping the newest keep of each. The registry keeps every upload of a name,
// and serves the newest; the configuration files are replaced every few days, so
// without this the old copies would pile up unseen. It needs the operator's own
// token: a deploy token cannot delete.
func (c *Client) PruneFiles(ctx context.Context, project int64, pkg, version string, keep int) (deleted int, err error) {
	if keep < 1 {
		return 0, errors.New("оставить нужно хотя бы одну копию каждого файла")
	}
	if c.Token == "" || c.Deploy {
		return 0, errors.New("чтобы убрать старые копии, нужен GITLAB_TOKEN с правом api")
	}
	pid, found, err := c.findPackage(ctx, project, pkg, version)
	if err != nil || !found {
		return 0, err
	}

	var all []packageFile
	for page := 1; ; page++ {
		var batch []packageFile
		addr := fmt.Sprintf("%s/api/v4/projects/%d/packages/%d/package_files?per_page=100&page=%d", c.Base, project, pid, page)
		if err := c.getJSON(ctx, addr, &batch); err != nil {
			return 0, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}

	byName := map[string][]packageFile{}
	for _, f := range all {
		byName[f.FileName] = append(byName[f.FileName], f)
	}
	for _, files := range byName {
		// Ids only grow, so the highest are the newest.
		sort.Slice(files, func(i, j int) bool { return files[i].ID > files[j].ID })
		if len(files) <= keep {
			continue
		}
		for _, old := range files[keep:] {
			addr := fmt.Sprintf("%s/api/v4/projects/%d/packages/%d/package_files/%d", c.Base, project, pid, old.ID)
			if err := c.delete(ctx, addr); err != nil {
				return deleted, err
			}
			deleted++
		}
	}
	return deleted, nil
}

// findPackage looks up a package of the exact name and version.
func (c *Client) findPackage(ctx context.Context, project int64, pkg, version string) (id int64, found bool, err error) {
	for page := 1; ; page++ {
		var batch []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		addr := fmt.Sprintf("%s/api/v4/projects/%d/packages?package_type=generic&package_name=%s&per_page=100&page=%d",
			c.Base, project, url.QueryEscape(pkg), page)
		if err := c.getJSON(ctx, addr, &batch); err != nil {
			return 0, false, err
		}
		for _, p := range batch {
			if p.Name == pkg && p.Version == version {
				return p.ID, true, nil
			}
		}
		if len(batch) < 100 {
			return 0, false, nil
		}
	}
}

func (c *Client) getJSON(ctx context.Context, addr string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gitlab GET %s: %s: %s", addr, resp.Status, firstLine(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("gitlab ответил на %s не так, как ожидалось", addr)
	}
	return nil
}

func (c *Client) delete(ctx context.Context, addr string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, addr, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gitlab DELETE %s: %s: %s", addr, resp.Status, firstLine(raw))
	}
	return nil
}
