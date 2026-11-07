// Command awgnode runs on the server. It brings up the AmneziaWG interface,
// turns the machine into a gateway and keeps a status file for awgctl to read.
//
// It needs root: it creates a tunnel device and writes nftables rules.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"awgrotator/internal/node"
)

func main() {
	configPath := flag.String("config", node.DefaultConfigPath, "path to the configuration file")
	debug := flag.Bool("debug", false, "log every command run and, with split routing on, every site name looked up (private: keep off)")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(log, *configPath); err != nil {
		log.Error("stopped", "error", err)
		fmt.Fprintf(os.Stderr, "awgnode: %v\n", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, configPath string) error {
	cfg, err := node.LoadConfig(configPath)
	if err != nil {
		return err
	}

	// SIGTERM is what systemd sends on stop and restart.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting", "interface", cfg.Interface, "peers", len(cfg.Peers))
	return node.New(cfg, log).Run(ctx)
}
