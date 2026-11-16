package cloud

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"awgrotator/internal/cherry"
)

func init() { register("cherry", openCherry) }

// cherryRegions maps our city codes to Cherry's region slugs.
var cherryRegions = map[string]string{
	"sia": "LT-Siauliai",
	"ams": "NL-Amsterdam",
	"fra": "DE-Frankfurt",
	"sto": "SE-Stockholm",
}

// cherryProvider keeps every server in one project of our own and also
// requires our tag, because a Cherry key reaches the whole account: the
// operator's own servers live on the same account and must never be seen.
type cherryProvider struct {
	c   *cherry.Client
	set Settings
}

func openCherry(s Settings) (Provider, error) { return NewCherry(cherry.New(s.Key), s) }

// NewCherry wraps an existing client; tests use it to point at a fake API.
func NewCherry(c *cherry.Client, s Settings) (Provider, error) {
	switch {
	case s.Project <= 0:
		return nil, errors.New("cherry: no project for our servers; run awgctl cloud setup cherry")
	case s.Tag == "":
		return nil, errors.New("cherry: no tag to mark our servers with")
	case s.Plan == "" || s.Image == "":
		return nil, errors.New("cherry: no plan or image")
	}
	return &cherryProvider{c: c, set: s}, nil
}

func (p *cherryProvider) Name() string { return "cherry" }

func (p *cherryProvider) Locations() []string {
	return slices.Sorted(maps.Keys(cherryRegions))
}

func (p *cherryProvider) Create(ctx context.Context, spec Spec) (*Server, error) {
	region, ok := cherryRegions[spec.Location]
	if !ok {
		return nil, fmt.Errorf("cherry has no city %q (it has %s)", spec.Location, strings.Join(p.Locations(), ", "))
	}
	billing := spec.Billing
	if billing == "" {
		billing = Hourly
	}
	keys := []string(nil)
	if spec.SSHKeyID != "" {
		keys = []string{spec.SSHKeyID}
	}
	s, err := p.c.CreateServer(ctx, p.set.Project, cherry.CreateRequest{
		Plan:          p.set.Plan,
		Region:        region,
		Image:         p.set.Image,
		Hostname:      spec.Label,
		SSHKeys:       keys,
		Tags:          map[string]string{p.set.Tag: spec.Label},
		Cycle:         string(billing),
		ConfigureIPv6: true,
	})
	if err != nil {
		if looksLikeNoCapacity(err.Error()) {
			return nil, fmt.Errorf("%w: %v", ErrNoCapacity, err)
		}
		return nil, err
	}
	out := p.server(*s)
	if out.Location == "" {
		out.Location = spec.Location
	}
	return &out, nil
}

func (p *cherryProvider) Get(ctx context.Context, id string) (*Server, error) {
	n, err := strconv.Atoi(id)
	if err != nil {
		return nil, fmt.Errorf("cherry/%s: %w", id, ErrNotFound)
	}
	s, err := p.c.Server(ctx, n)
	if err != nil {
		if errors.Is(err, cherry.ErrNotFound) {
			return nil, fmt.Errorf("cherry/%s: %w", id, ErrNotFound)
		}
		return nil, err
	}
	if !p.ours(*s) {
		return nil, fmt.Errorf("cherry/%s: %w", id, ErrNotOurs)
	}
	out := p.server(*s)
	return &out, nil
}

// List looks only inside our project, and keeps only what carries our tag.
func (p *cherryProvider) List(ctx context.Context) ([]Server, error) {
	servers, err := p.c.Servers(ctx, p.set.Project)
	if err != nil {
		return nil, err
	}
	var out []Server
	for _, s := range servers {
		if p.ours(s) {
			out = append(out, p.server(s))
		}
	}
	return out, nil
}

// Delete reads the server first and refuses one that is not in our project
// with our tag, whatever id it was given.
func (p *cherryProvider) Delete(ctx context.Context, id string) error {
	if _, err := p.Get(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	n, _ := strconv.Atoi(id)
	err := p.c.DeleteServer(ctx, n)
	if errors.Is(err, cherry.ErrNotFound) {
		return nil
	}
	return err
}

func (p *cherryProvider) EnsureSSHKey(ctx context.Context, label, pub string) (string, error) {
	keys, err := p.c.SSHKeys(ctx)
	if err != nil {
		return "", fmt.Errorf("list SSH keys: %w", err)
	}
	for _, k := range keys {
		if strings.TrimSpace(k.Key) == strings.TrimSpace(pub) {
			return strconv.Itoa(k.ID), nil
		}
	}
	id, err := p.c.CreateSSHKey(ctx, label, pub)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(id), nil
}

func (p *cherryProvider) ours(s cherry.Server) bool {
	if s.Project.ID != p.set.Project {
		return false
	}
	_, tagged := s.Tags[p.set.Tag]
	return tagged
}

func (p *cherryProvider) server(s cherry.Server) Server {
	created, _ := time.Parse(time.RFC3339, s.Created)
	var location string
	for code, slug := range cherryRegions {
		if slug == s.Region.Slug {
			location = code
		}
	}
	return Server{
		Provider: "cherry",
		ID:       strconv.Itoa(s.ID),
		Location: location,
		IP:       s.PublicIPv4(),
		IPv6:     s.PublicIPv6(),
		Label:    s.Hostname,
		Created:  created,
		Ready:    s.Ready(),
	}
}

// CherrySetup finds the team, makes our project if it does not exist yet, and
// returns its id. The operator's tool runs it once.
func CherrySetup(ctx context.Context, key, projectName string) (int, error) {
	c := cherry.New(key)
	teams, err := c.Teams(ctx)
	if err != nil {
		return 0, fmt.Errorf("list teams: %w", err)
	}
	if len(teams) != 1 {
		return 0, fmt.Errorf("the key belongs to %d teams; expected exactly one", len(teams))
	}
	projects, err := c.Projects(ctx, teams[0].ID)
	if err != nil {
		return 0, fmt.Errorf("list projects: %w", err)
	}
	for _, pr := range projects {
		if pr.Name == projectName {
			return pr.ID, nil
		}
	}
	pr, err := c.CreateProject(ctx, teams[0].ID, projectName)
	if err != nil {
		return 0, fmt.Errorf("create project %s: %w", projectName, err)
	}
	return pr.ID, nil
}
