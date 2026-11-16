package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"awgrotator/internal/cloud"
	"awgrotator/internal/config"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/store"
)

// cloudSettings are the hosting providers the operator has credentials for,
// with what servers are made of and the tag they carry. Cherry is included
// once it has been set up with "awgctl cloud setup cherry".
func cloudSettings(st *store.State, secrets config.Secrets, tag string) map[string]cloud.Settings {
	out := map[string]cloud.Settings{
		"vultr": {
			Key:      secrets.VultrAPIKey,
			Plan:     config.Plan,
			Image:    strconv.Itoa(config.OsID),
			Tag:      tag,
			SSHKeyID: st.SSHKeyIDs["vultr"],
		},
	}
	if secrets.CherryAPIKey != "" && st.CherryProjectID > 0 {
		out["cherry"] = cloud.Settings{
			Key:      secrets.CherryAPIKey,
			Plan:     config.CherryPlan,
			Image:    config.CherryImage,
			Tag:      tag,
			Project:  st.CherryProjectID,
			SSHKeyID: st.SSHKeyIDs["cherry"],
		}
	}
	return out
}

// openClouds opens every provider in cloudSettings.
func openClouds(st *store.State, secrets config.Secrets, tag string) (cloud.Set, error) {
	set := cloud.Set{}
	for name, s := range cloudSettings(st, secrets, tag) {
		p, err := cloud.Open(name, s)
		if err != nil {
			return nil, err
		}
		set[name] = p
	}
	return set, nil
}

// placesFor are the places a line of servers moves between: the Vultr cities
// by the hour, and the Cherry ones by the month when Cherry is set up.
func placesFor(clouds map[string]cloud.Settings) []lifecycle.PlaceConfig {
	var out []lifecycle.PlaceConfig
	for _, r := range config.Regions {
		out = append(out, lifecycle.PlaceConfig{ID: "vultr:" + r, Billing: cloud.Hourly})
	}
	if _, ok := clouds["cherry"]; ok {
		for _, r := range config.CherryLocations {
			out = append(out, lifecycle.PlaceConfig{ID: "cherry:" + r, Billing: cloud.Monthly})
		}
	}
	return out
}

// pickPlace chooses where a server built from this machine goes. A place that
// is asked for is used as it is; otherwise one paid by the hour, other than
// the current one, is picked at random.
func pickPlace(want string, places []lifecycle.PlaceConfig, current cloud.Place) (lifecycle.PlaceConfig, error) {
	cfg := lifecycle.Config{Places: places}
	if want != "" {
		return cfg.FindPlace(want)
	}
	var choices []lifecycle.PlaceConfig
	for _, p := range places {
		if p.Billing == cloud.Hourly && p.Place() != current {
			choices = append(choices, p)
		}
	}
	if len(choices) == 0 {
		return lifecycle.PlaceConfig{}, errors.New("нет мест с почасовой оплатой; укажи место явно: -place")
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(choices))))
	if err != nil {
		return lifecycle.PlaceConfig{}, err
	}
	return choices[n.Int64()], nil
}

// ensureSSHKey registers our public key with a provider once and remembers
// its id in the state.
func ensureSSHKey(ctx context.Context, p cloud.Provider, st *store.State, root string, logf func(string, ...any)) (string, error) {
	if id := st.SSHKeyIDs[p.Name()]; id != "" {
		return id, nil
	}
	logf("Регистрирую ключ SSH в %s…", p.Name())
	id, err := p.EnsureSSHKey(ctx, "awg-rotator", st.SSHPublic)
	if err != nil {
		return "", fmt.Errorf("регистрация ключа SSH в %s: %w", p.Name(), err)
	}
	if st.SSHKeyIDs == nil {
		st.SSHKeyIDs = map[string]string{}
	}
	st.SSHKeyIDs[p.Name()] = id
	if err := st.Save(root); err != nil {
		return "", err
	}
	return id, nil
}

// serverLine is one of our servers for a listing.
func serverLine(s cloud.Server) string {
	return fmt.Sprintf("%-6s %-10s %-4s %s", s.Provider, shortID(s.ID), s.Location, s.IP)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ---- cloud

func cmdCloud(args []string) error {
	if len(args) < 2 || args[0] != "setup" || args[1] != "cherry" {
		return errors.New("использование: awgctl cloud setup cherry")
	}
	fs := flag.NewFlagSet("cloud setup cherry", flag.ExitOnError)
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}

	root, err := config.Root()
	if err != nil {
		return err
	}
	st, err := store.Load(root)
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets(root)
	if err != nil {
		return err
	}
	if secrets.CherryAPIKey == "" {
		return fmt.Errorf("впиши ключ Cherry в %s: %s=…", config.SecretsFile, config.CherryKeyName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logf := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }

	logf("Ищу проект %s в Cherry…", config.CherryProject)
	project, err := cloud.CherrySetup(ctx, secrets.CherryAPIKey, config.CherryProject)
	if err != nil {
		return err
	}
	st.CherryProjectID = project
	if err := st.Save(root); err != nil {
		return err
	}
	logf("  проект %d: наши серверы живут только там", project)

	set, err := openClouds(st, secrets, config.InstanceTag)
	if err != nil {
		return err
	}
	p, err := set.Get("cherry")
	if err != nil {
		return err
	}
	id, err := ensureSSHKey(ctx, p, st, root, logf)
	if err != nil {
		return err
	}
	logf("  ключ SSH зарегистрирован (%s)", id)

	ours, err := p.List(ctx)
	if err != nil {
		return err
	}
	logf("  наших серверов в Cherry: %d", len(ours))
	logf("")
	logf("Готово. Места Cherry (%s) появятся на работающем сервере после awgctl update.",
		strings.Join(prefixed("cherry:", config.CherryLocations), ", "))
	logf("Туда переезжают только по вашей команде: оплата помесячная.")
	return nil
}

func prefixed(p string, list []string) []string {
	out := slices.Clone(list)
	for i := range out {
		out[i] = p + out[i]
	}
	return out
}
