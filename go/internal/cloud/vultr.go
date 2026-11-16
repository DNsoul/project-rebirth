package cloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"awgrotator/internal/vultr"
)

func init() { register("vultr", openVultr) }

// vultrLocations are the cities used on Vultr. Our city codes are Vultr's own
// region ids, which is where they came from.
var vultrLocations = []string{"sto", "waw", "fra", "ams"}

type vultrProvider struct {
	c   *vultr.Client
	set Settings
	os  int
}

func openVultr(s Settings) (Provider, error) { return NewVultr(vultr.New(s.Key), s) }

// NewVultr wraps an existing client; tests use it to point at a fake API.
func NewVultr(c *vultr.Client, s Settings) (Provider, error) {
	os, err := strconv.Atoi(s.Image)
	if err != nil || os <= 0 {
		return nil, fmt.Errorf("vultr: the image %q is not an OS id", s.Image)
	}
	if s.Tag == "" {
		return nil, errors.New("vultr: no tag to mark our servers with")
	}
	return &vultrProvider{c: c, set: s, os: os}, nil
}

func (p *vultrProvider) Name() string        { return "vultr" }
func (p *vultrProvider) Locations() []string { return slices.Clone(vultrLocations) }

func (p *vultrProvider) Create(ctx context.Context, spec Spec) (*Server, error) {
	if spec.Billing == Monthly {
		return nil, errors.New("vultr bills by the hour only")
	}
	keys := []string(nil)
	if spec.SSHKeyID != "" {
		keys = []string{spec.SSHKeyID}
	}
	inst, err := p.c.CreateInstance(ctx, vultr.CreateRequest{
		Region:     spec.Location,
		Plan:       p.set.Plan,
		OsID:       p.os,
		Label:      spec.Label,
		Hostname:   spec.Label,
		Tags:       []string{p.set.Tag},
		SSHKeys:    keys,
		EnableIPv6: true,
		Backups:    "disabled",
	})
	if err != nil {
		if looksLikeNoCapacity(err.Error()) {
			return nil, fmt.Errorf("%w: %v", ErrNoCapacity, err)
		}
		return nil, err
	}
	s := p.server(*inst)
	return &s, nil
}

func (p *vultrProvider) Get(ctx context.Context, id string) (*Server, error) {
	inst, err := p.c.Instance(ctx, id)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil, fmt.Errorf("vultr/%s: %w", id, ErrNotFound)
		}
		return nil, err
	}
	if !slices.Contains(inst.Tags, p.set.Tag) {
		return nil, fmt.Errorf("vultr/%s: %w", id, ErrNotOurs)
	}
	s := p.server(*inst)
	return &s, nil
}

// List asks Vultr for the tagged servers only: an instance without the tag is
// not merely skipped but never seen.
func (p *vultrProvider) List(ctx context.Context) ([]Server, error) {
	insts, err := p.c.InstancesByTag(ctx, p.set.Tag)
	if err != nil {
		return nil, err
	}
	out := make([]Server, 0, len(insts))
	for _, i := range insts {
		out = append(out, p.server(i))
	}
	return out, nil
}

func (p *vultrProvider) Delete(ctx context.Context, id string) error {
	if _, err := p.Get(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	err := p.c.DeleteInstance(ctx, id)
	if err != nil && strings.Contains(err.Error(), "404") {
		return nil
	}
	return err
}

func (p *vultrProvider) EnsureSSHKey(ctx context.Context, label, pub string) (string, error) {
	keys, err := p.c.SSHKeys(ctx)
	if err != nil {
		return "", fmt.Errorf("list SSH keys: %w", err)
	}
	for _, k := range keys {
		if strings.TrimSpace(k.SSHKey) == strings.TrimSpace(pub) {
			return k.ID, nil
		}
	}
	return p.c.CreateSSHKey(ctx, label, pub)
}

func (p *vultrProvider) server(i vultr.Instance) Server {
	created, _ := time.Parse(time.RFC3339, i.DateCreated)
	ip := i.MainIP
	if ip == "0.0.0.0" {
		ip = ""
	}
	return Server{
		Provider: "vultr",
		ID:       i.ID,
		Location: i.Region,
		IP:       ip,
		IPv6:     i.V6MainIP,
		Label:    i.Label,
		Created:  created,
		Ready:    i.Ready(),
	}
}
