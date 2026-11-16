package lifecycle

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"awgrotator/internal/cloud"
)

// PlaceConfig is a place a new server may go and how it is paid for there.
type PlaceConfig struct {
	ID      string        `json:"id"` // provider:city
	Billing cloud.Billing `json:"billing"`
}

// Place returns the parsed place.
func (p PlaceConfig) Place() cloud.Place {
	pl, _ := cloud.ParsePlace(p.ID)
	return pl
}

// MonthlyMargin is how long before the end of a paid month a monthly server
// leaves, so that a move that takes a while still finishes before the next
// month is charged.
const MonthlyMargin = 12 * time.Hour

// migrate turns a version 1 configuration, which knew only Vultr, into the
// current layout. A version 1 file still on a server is read this way, so an
// update does not have to rewrite it first.
func (c *Config) migrate() {
	if c.Version != 1 {
		return
	}
	if c.Clouds == nil && c.LegacyVultrKey != "" {
		c.Clouds = map[string]cloud.Settings{"vultr": {
			Key:      c.LegacyVultrKey,
			Plan:     c.LegacyPlan,
			Image:    strconv.Itoa(c.LegacyOsID),
			Tag:      c.LegacyTag,
			SSHKeyID: c.LegacySSHKeyID,
		}}
	}
	if c.Places == nil {
		for _, r := range c.LegacyRegions {
			c.Places = append(c.Places, PlaceConfig{ID: "vultr:" + r, Billing: cloud.Hourly})
		}
	}
	if c.Provider == "" {
		c.Provider = "vultr"
	}
	if c.Billing == "" {
		c.Billing = cloud.Hourly
	}
	c.LegacyVultrKey, c.LegacySSHKeyID, c.LegacyPlan, c.LegacyTag = "", "", "", ""
	c.LegacyOsID, c.LegacyRegions = 0, nil
	c.Version = ConfigVersion
}

func (c *Config) validateClouds() error {
	if len(c.Clouds) == 0 {
		return errors.New("manager configuration has no hosting provider")
	}
	locations := map[string][]string{}
	for name, s := range c.Clouds {
		p, err := cloud.Open(name, s)
		if err != nil {
			return fmt.Errorf("hosting provider %s: %w", name, err)
		}
		if s.SSHKeyID == "" {
			return fmt.Errorf("hosting provider %s: no ssh key id", name)
		}
		locations[name] = p.Locations()
	}
	if _, ok := c.Clouds[c.Provider]; !ok {
		return fmt.Errorf("this server's provider %q is not among the configured ones", c.Provider)
	}
	if !c.Billing.Valid() {
		return fmt.Errorf("this server's billing %q is neither hourly nor monthly", c.Billing)
	}

	seen := map[string]bool{}
	for _, pc := range c.Places {
		pl, err := cloud.ParsePlace(pc.ID)
		if err != nil {
			return err
		}
		if !strings.Contains(pc.ID, ":") {
			return fmt.Errorf("place %q: write it as provider:city", pc.ID)
		}
		known, ok := locations[pl.Provider]
		if !ok {
			return fmt.Errorf("place %s: provider %s is not configured", pc.ID, pl.Provider)
		}
		if !slices.Contains(known, pl.Location) {
			return fmt.Errorf("place %s: %s has no such city (it has %s)", pc.ID, pl.Provider, strings.Join(known, ", "))
		}
		if !pc.Billing.Valid() {
			return fmt.Errorf("place %s: billing %q is neither hourly nor monthly", pc.ID, pc.Billing)
		}
		if seen[pc.ID] {
			return fmt.Errorf("place %s is listed twice", pc.ID)
		}
		seen[pc.ID] = true
	}
	if len(c.Places) < 2 {
		return errors.New("manager configuration needs at least two places to move between")
	}
	return nil
}

// CurrentPlace is where this server is.
func (c *Config) CurrentPlace() cloud.Place {
	return cloud.Place{Provider: c.Provider, Location: c.Region}
}

// PlaceIDs lists the places, in order.
func (c *Config) PlaceIDs() []string {
	out := make([]string, len(c.Places))
	for i, p := range c.Places {
		out[i] = p.ID
	}
	return out
}

// FindPlace resolves what the operator asked for: a full place, or a bare
// city, which means the first place in that city, hourly ones first.
func (c *Config) FindPlace(want string) (PlaceConfig, error) {
	want = strings.TrimSpace(want)
	if strings.Contains(want, ":") {
		for _, p := range c.Places {
			if p.ID == want {
				return p, nil
			}
		}
		return PlaceConfig{}, fmt.Errorf("unknown place %q, choose from %s", want, strings.Join(c.PlaceIDs(), ", "))
	}
	var found []PlaceConfig
	for _, p := range c.Places {
		if p.Place().Location == want {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		return PlaceConfig{}, fmt.Errorf("unknown place %q, choose from %s", want, strings.Join(c.PlaceIDs(), ", "))
	}
	slices.SortStableFunc(found, func(a, b PlaceConfig) int {
		return boolRank(a.Billing != cloud.Hourly) - boolRank(b.Billing != cloud.Hourly)
	})
	return found[0], nil
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// PaidUntil is when the paid month of a monthly server ends, or zero for a
// server paid by the hour.
func (c *Config) PaidUntil() time.Time {
	if c.Billing != cloud.Monthly {
		return time.Time{}
	}
	return c.CreatedTime().Add(cloud.PaidPeriod)
}

// Providers opens every configured provider.
func (c *Config) Providers() (cloud.Set, error) {
	set := cloud.Set{}
	for name, s := range c.Clouds {
		p, err := cloud.Open(name, s)
		if err != nil {
			return nil, fmt.Errorf("hosting provider %s: %w", name, err)
		}
		set[name] = p
	}
	return set, nil
}
