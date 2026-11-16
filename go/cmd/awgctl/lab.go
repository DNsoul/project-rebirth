package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"awgrotator/internal/cloud"
	"awgrotator/internal/config"
	"awgrotator/internal/remote"
	"awgrotator/internal/store"
)

// LabTag marks the throwaway servers used for the blocking experiment. It is
// deliberately different from the production tag, so "awgctl down" can never
// reach a lab server and "awgctl lab-down" can never reach a real one.
const LabTag = "awg-lab"

const (
	probeBinaryPath = "/usr/local/bin/awgprobe"
	probeUnitPath   = "/etc/systemd/system/awgprobe.service"
	probeLogPath    = "/var/log/awgprobe.jsonl"
)

func cmdLabUp(args []string) error {
	fs := flag.NewFlagSet("lab-up", flag.ExitOnError)
	placeFlag := fs.String("place", "vultr:fra", "место: хостер:город (опытные серверы всегда почасовые)")
	withProbe := fs.Bool("probe", false, "поставить зонд (иначе сервер остаётся пустым)")
	label := fs.String("label", "", "метка (по умолчанию по роли и региону)")
	udp := fs.String("udp", "53,443,34567", "UDP-порты зонда")
	if err := fs.Parse(args); err != nil {
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

	role := "bare"
	if *withProbe {
		role = "probe"
	}
	where, err := cloud.ParsePlace(*placeFlag)
	if err != nil {
		return err
	}
	region := &where.Location
	name := *label
	if name == "" {
		name = fmt.Sprintf("lab-%s-%s", role, *region)
	}

	var binary []byte
	if *withProbe {
		fmt.Println("Собираю зонд под Linux…")
		binary, err = buildLinux(root, "awgprobe")
		if err != nil {
			return err
		}
		fmt.Printf("  %d КБ\n", len(binary)/1024)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	labs, err := openClouds(st, secrets, LabTag)
	if err != nil {
		return err
	}
	p, err := labs.Get(where.Provider)
	if err != nil {
		return err
	}
	keyID, err := ensureSSHKey(ctx, p, st, root, func(f string, a ...any) { fmt.Printf(f+"\n", a...) })
	if err != nil {
		return err
	}
	signer, err := st.SSHSigner()
	if err != nil {
		return err
	}

	fmt.Printf("Создаю сервер %s в %s…\n", role, where)
	inst, err := p.Create(ctx, cloud.Spec{Location: *region, Label: name, SSHKeyID: keyID, Billing: cloud.Hourly})
	if err != nil {
		return err
	}

	id := inst.ID
	inst, err = cloud.WaitReady(ctx, p, id)
	if err != nil {
		return fmt.Errorf("сервер %s создан, но не поднялся: %w", id, err)
	}
	fmt.Printf("  %s  %s  (IPv6 %s)\n", shortID(inst.ID), inst.IP, inst.IPv6)

	if !*withProbe {
		fmt.Println("\nПустой контроль готов. Его никто не настраивает и ничего на нём не слушает,")
		fmt.Println("кроме SSH. Если он тоже отвалится — дело не в нашем трафике.")
		return nil
	}

	fmt.Println("  жду SSH…")
	sshCtx, cancelSSH := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelSSH()
	cli, err := remote.WaitForSSH(sshCtx, inst.IP, "root", signer, "")
	if err != nil {
		return err
	}
	defer cli.Close()

	if err := installProbe(cli, binary, inst.IP, *udp); err != nil {
		return err
	}

	out, err := cli.Run("systemctl is-active awgprobe; ss -lun | grep -c . ")
	if err == nil {
		fmt.Printf("  служба: %s\n", out)
	}

	fmt.Printf("\nЗонд работает на %s\n", inst.IP)
	fmt.Printf("Журнал зонда: awgctl run -host %s 'tail -3 %s'\n", inst.IP, probeLogPath)
	return nil
}

func installProbe(cli *remote.Client, binary []byte, bindIP, udpPorts string) error {
	unit := fmt.Sprintf(`[Unit]
Description=awgprobe
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s -bind %s -udp %s -tcp 443 -log %s -every 30s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, probeBinaryPath, bindIP, udpPorts, probeLogPath)

	for _, f := range []struct {
		what    string
		path    string
		mode    string
		content []byte
	}{
		{"зонд", probeBinaryPath, "0700", binary},
		{"службу", probeUnitPath, "0644", []byte(unit)},
	} {
		fmt.Printf("  загружаю %s (%d КБ)\n", f.what, len(f.content)/1024)
		if err := cli.Upload(f.path, f.mode, f.content); err != nil {
			return err
		}
	}

	if _, err := cli.Run("systemctl daemon-reload && systemctl enable --now awgprobe"); err != nil {
		journal, _ := cli.Run("journalctl -u awgprobe -n 30 --no-pager")
		return fmt.Errorf("зонд не запустился: %w\n--- журнал ---\n%s", err, journal)
	}
	return nil
}

func cmdLabList(args []string) error {
	fs := flag.NewFlagSet("lab-list", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := config.Root()
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	st, err := store.Load(root)
	if err != nil {
		return err
	}
	labs, err := openClouds(st, secrets, LabTag)
	if err != nil {
		return err
	}
	ours, err := labs.List(ctx)
	if err != nil {
		return err
	}
	if len(ours) == 0 {
		fmt.Println("опытных серверов нет")
		return nil
	}
	for _, i := range ours {
		ready := "строится"
		if i.Ready {
			ready = "готов"
		}
		fmt.Printf("%s %-24s %s\n", serverLine(i), i.Label, ready)
	}

	// Ready to paste into the watcher.
	fmt.Print("\nдля наблюдателя: -targets '")
	for n, i := range ours {
		if n > 0 {
			fmt.Print(",")
		}
		fmt.Printf("%s=%s", i.Label, i.IP)
	}
	fmt.Println("'")
	return nil
}

func cmdLabDown(args []string) error {
	fs := flag.NewFlagSet("lab-down", flag.ExitOnError)
	yes := fs.Bool("yes", false, "подтвердить удаление")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := config.Root()
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets(root)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := store.Load(root)
	if err != nil {
		return err
	}
	labs, err := openClouds(st, secrets, LabTag)
	if err != nil {
		return err
	}

	ours, err := labs.List(ctx)
	if err != nil {
		return err
	}
	if len(ours) == 0 {
		fmt.Println("опытных серверов нет")
		return nil
	}
	fmt.Println("Будут удалены:")
	for _, i := range ours {
		fmt.Printf("  %s %s\n", serverLine(i), i.Label)
	}
	if !*yes {
		return errors.New("повтори с -yes")
	}
	for _, i := range ours {
		if err := labs.Delete(ctx, i.Provider, i.ID); err != nil {
			fmt.Printf("  %s: %v\n", i.Ref(), err)
			continue
		}
		fmt.Printf("  %s удалён\n", i.Ref())
	}
	return nil
}

// buildLinux cross-compiles one of this project's commands for the servers.
func buildLinux(root, name string) ([]byte, error) {
	out := filepath.Join(root, "dist", name+"-linux-amd64")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("go"); err == nil {
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/"+name)
		cmd.Dir = filepath.Join(root, "go")
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		if combined, buildErr := cmd.CombinedOutput(); buildErr != nil {
			return nil, fmt.Errorf("сборка %s не удалась: %w\n%s", name, buildErr, combined)
		}
	}
	binary, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("нет собранного %s (%s) и Go недоступен: %w", name, out, err)
	}
	return binary, nil
}
