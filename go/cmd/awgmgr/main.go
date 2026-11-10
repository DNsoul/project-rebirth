// Command awgmgr is a server's caretaker. It runs next to awgnode, watches the
// server's health, reads the operator's instructions from the gist, and builds,
// tests and hands over to a replacement when it is time.
//
// It holds the credentials needed to create servers, so it runs as root from a
// file only root can read, and it logs states and counts, never site names or
// addresses of users.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"awgrotator/internal/globalping"
	"awgrotator/internal/lifecycle"
	"awgrotator/internal/stores"
	"awgrotator/internal/vultr"
)

func main() {
	configPath := flag.String("config", lifecycle.DefaultConfigPath, "path to the manager configuration")
	tickEvery := flag.Duration("tick", 30*time.Second, "how often to check what is due")
	debug := flag.Bool("debug", false, "verbose logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(log, *configPath, *tickEvery); err != nil {
		log.Error("stopped", "error", err)
		fmt.Fprintf(os.Stderr, "awgmgr: %v\n", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, configPath string, tickEvery time.Duration) error {
	cfg, err := lifecycle.LoadConfig(configPath)
	if err != nil {
		return err
	}
	journal, err := lifecycle.OpenJournal(filepath.Join(cfg.Data(), "journal.json"))
	if err != nil {
		return err
	}

	vc := vultr.New(cfg.VultrKey)
	cloud := &lifecycle.VultrCloud{Client: vc, Tag: cfg.Tag}
	host := &lifecycle.SystemHost{Cfg: cfg}
	// The gist and, when this server has been given one, the GitLab package are
	// equal places: everything is written to both, and whichever is behind is
	// caught up from the other.
	var gl *stores.GitLabAccess
	if g := cfg.GitLab; g != nil {
		gl = &stores.GitLabAccess{Base: g.Base, Project: g.Project, Package: g.Package, Token: g.Token, Deploy: true}
	}
	pub := &lifecycle.StorePublisher{
		Cfg:    cfg,
		Stores: stores.NewSet(cfg.Trust(), cfg.GitHubToken, cfg.GistID, gl),
		J:      journal,
		Log:    log,
	}

	progress := func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) }
	engine := &lifecycle.Engine{
		Cfg:    cfg,
		J:      journal,
		Cloud:  cloud,
		Prov:   &lifecycle.DeployProvisioner{Cfg: cfg, Client: vc, Logf: progress},
		Tester: lifecycle.TunnelTester{},
		Pub:    pub,
		Host:   host,
		Plan:   lifecycle.PlanFunc(cfg, progress),
		Log:    log,
	}
	manager := lifecycle.NewManager(lifecycle.Manager{
		Cfg:    cfg,
		J:      journal,
		Eng:    engine,
		Pub:    pub,
		Cloud:  cloud,
		Host:   host,
		Prober: &lifecycle.GlobalpingProber{Client: globalping.New()},

		// The availability log: whether this server can still get answers from
		// Russia, written down for the record. Nothing acts on it.
		Watch:   &lifecycle.WatchLog{Path: filepath.Join(cfg.Data(), lifecycle.WatchLogName)},
		Reverse: lifecycle.NewNetProber(),

		Log: log,
	})

	// SIGTERM is what systemd sends on stop and restart.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting", "generation", cfg.Generation, "region", cfg.Region,
		"auto_age", cfg.Policy.AutoAge, "auto_block", cfg.Policy.AutoBlock)
	if err := manager.Start(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	for {
		if err := manager.Tick(ctx); err != nil && ctx.Err() == nil {
			// A tick that fails is retried on the next one. The failure is
			// logged, and nothing else is done: the process stays up, because
			// restarting it would only repeat the same step sooner.
			log.Error("tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			log.Info("stopping")
			return nil
		case <-time.After(tickEvery):
		}
	}
}
