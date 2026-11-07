// Command awgsoak runs several kinds of tunnel side by side from this machine,
// carries realistic traffic through each for as long as it lives, and records
// when each one stops working.
//
//	awgsoak run    -config soak.json     # runs until interrupted
//	awgsoak report -dir soak-logs        # what the logs say so far
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"awgrotator/internal/soak"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run(os.Args[2:])
	case "report":
		err = report(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "awgsoak:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "использование: awgsoak run -config soak.json | awgsoak report -dir soak-logs")
	os.Exit(2)
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", "soak.json", "файл с описанием туннелей")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := soak.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fmt.Printf("Запускаю %d туннелей, журналы в %s. Остановить: Ctrl+C.\n", len(cfg.Arms), cfg.LogDir)
	return soak.Run(ctx, cfg, os.Stdout)
}

func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dir := fs.String("dir", "soak-logs", "папка с журналами")
	if err := fs.Parse(args); err != nil {
		return err
	}
	reports, err := soak.Summarize(*dir)
	if err != nil {
		return err
	}
	if len(reports) == 0 {
		return fmt.Errorf("в %s нет журналов", *dir)
	}
	fmt.Print(soak.Render(reports))
	return nil
}
