// Package deploy creates a server and installs the node on it.
//
// The same code runs from the operator's machine today and, from phase 3,
// inside a server that is building its own replacement. That is why it takes
// the node binary as bytes: a server copies the one it is already running
// rather than needing a compiler.
package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"awgrotator/internal/node"
	"awgrotator/internal/remote"
	"awgrotator/internal/vultr"
)

// Paths on the server.
const (
	BinaryPath = "/usr/local/bin/awgnode"
	ConfigPath = node.DefaultConfigPath
	UnitPath   = "/etc/systemd/system/awgnode.service"
	ScriptPath = "/root/.awgnode-install.sh"
)

// Timeouts for the slow parts.
const (
	ProvisionTimeout = 6 * time.Minute
	SSHTimeout       = 3 * time.Minute
	StatusTimeout    = 45 * time.Second
)

// Request is one server to build.
type Request struct {
	Region   string
	Plan     string
	OsID     int
	Tag      string
	Label    string
	SSHKeyID string

	// Binary is the linux/amd64 awgnode executable.
	Binary []byte

	// Config is what the node will run with.
	Config *node.Config

	// Files are uploaded alongside the node before it starts: the local
	// address set and the rules, when split routing is on.
	Files []File

	// FilesFor, when set, supplies more files once the instance exists. It
	// is for files that cannot be made earlier because they describe the
	// instance itself, such as the manager's configuration, which carries the
	// instance's own identifier.
	FilesFor func(inst *vultr.Instance) ([]File, error)

	// Services are extra systemd units to enable and start after the node,
	// for example the manager.
	Services []string

	// OnCreated is called as soon as the instance exists, before anything is
	// installed on it. A caller that must be able to find and remove the
	// instance if it dies partway through records its identifier here:
	// installing takes minutes, and an instance nobody has written down is
	// one nobody can clean up.
	OnCreated func(inst *vultr.Instance)

	// Signer authenticates to the new server as root.
	Signer ssh.Signer

	// HostKey is the expected host key, or empty to accept and record
	// whatever the new server presents.
	HostKey string
}

// File is an extra file to place on the server.
type File struct {
	What    string // for the progress message
	Path    string
	Mode    string
	Content []byte
}

// Result describes the server that is now running.
type Result struct {
	InstanceID string
	IP         string
	IPv6       string
	Region     string
	HostKey    string
	Status     node.Status
}

// Logf receives progress messages.
type Logf func(format string, args ...any)

// Provision creates the instance, installs the node and waits until it
// reports itself up.
//
// On any failure after the instance exists, the instance is destroyed: a half
// built server that nothing knows about would keep costing money.
func Provision(ctx context.Context, vc *vultr.Client, req Request, logf Logf) (res *Result, err error) {
	if err := req.Config.Validate(); err != nil {
		return nil, fmt.Errorf("node configuration: %w", err)
	}
	if len(req.Binary) == 0 {
		return nil, fmt.Errorf("no node binary to install")
	}

	logf("создаю сервер в регионе %s…", req.Region)
	inst, err := vc.CreateInstance(ctx, vultr.CreateRequest{
		Region:     req.Region,
		Plan:       req.Plan,
		OsID:       req.OsID,
		Label:      req.Label,
		Hostname:   req.Label,
		Tags:       []string{req.Tag},
		SSHKeys:    []string{req.SSHKeyID},
		EnableIPv6: true,
		Backups:    "disabled",
	})
	if err != nil {
		return nil, fmt.Errorf("create instance: %w", err)
	}

	if req.OnCreated != nil {
		req.OnCreated(inst)
	}

	defer func() {
		if err == nil {
			return
		}
		logf("не получилось — удаляю сервер %s, чтобы он не висел на счёте", inst.ID[:8])
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if delErr := vc.DeleteInstance(cleanup, inst.ID); delErr != nil {
			// Report it loudly: a leaked instance costs money.
			err = fmt.Errorf("%w\nВНИМАНИЕ: сервер %s удалить не удалось (%v), удали его сам", err, inst.ID, delErr)
		}
	}()

	logf("сервер %s создан, жду адрес…", inst.ID[:8])
	readyCtx, cancelReady := context.WithTimeout(ctx, ProvisionTimeout)
	defer cancelReady()
	inst, err = vc.WaitReady(readyCtx, inst.ID)
	if err != nil {
		return nil, err
	}
	logf("адрес %s, жду SSH…", inst.MainIP)

	sshCtx, cancelSSH := context.WithTimeout(ctx, SSHTimeout)
	defer cancelSSH()
	cli, err := remote.WaitForSSH(sshCtx, inst.MainIP, "root", req.Signer, req.HostKey)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	logf("SSH отвечает, ставлю узел…")

	extra := req.Files
	if req.FilesFor != nil {
		more, err := req.FilesFor(inst)
		if err != nil {
			return nil, fmt.Errorf("prepare the files for the new server: %w", err)
		}
		extra = append(append([]File(nil), extra...), more...)
	}
	if err := install(cli, req, extra, logf); err != nil {
		return nil, err
	}

	status, err := waitStatus(ctx, cli, req.Config.Status(), logf)
	if err != nil {
		return nil, err
	}
	// The status file proves the device was configured; this proves the
	// kernel really handed us the UDP port.
	if err := verifyListening(cli, req.Config.ListenPort); err != nil {
		return nil, err
	}
	// Give the resolver a moment to finish starting, then prove it works.
	if req.Config.Split != nil {
		status, err = waitSplit(ctx, cli, req.Config, logf)
		if err != nil {
			return nil, err
		}
		if err := VerifySplit(cli, req.Config, status, logf); err != nil {
			return nil, err
		}
	}

	return &Result{
		InstanceID: inst.ID,
		IP:         inst.MainIP,
		IPv6:       inst.V6MainIP,
		Region:     inst.Region,
		HostKey:    cli.HostKey(),
		Status:     status,
	}, nil
}

// installScript makes sure nft exists and starts the service.
//
// The nftables *service* is deliberately left alone: it would load
// /etc/nftables.conf and flush the table the node installs. Only the nft
// command itself is needed.
func installScript(extraServices []string) string {
	var b strings.Builder
	b.WriteString(`set -eu
if ! command -v nft >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  installed=0
  for attempt in 1 2 3 4 5 6; do
    if apt-get update -qq && apt-get install -y -qq nftables; then installed=1; break; fi
    echo "apt busy, retrying ($attempt)"
    sleep 5
  done
  [ "$installed" = 1 ] || { echo "could not install nftables"; exit 1; }
fi
systemctl daemon-reload
systemctl enable awgnode >/dev/null
systemctl restart awgnode
`)
	// Started after the node, which they depend on.
	for _, svc := range extraServices {
		fmt.Fprintf(&b, "systemctl enable %[1]s >/dev/null\nsystemctl restart %[1]s\n", svc)
	}
	return b.String()
}

func unitFile() string {
	return fmt.Sprintf(`[Unit]
Description=AmneziaWG node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s -config %s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, BinaryPath, ConfigPath)
}

func install(cli *remote.Client, req Request, extra []File, logf Logf) error {
	configJSON, err := req.Config.Marshal()
	if err != nil {
		return err
	}

	files := []File{
		{"узел", BinaryPath, "0700", req.Binary},
		{"конфиг", ConfigPath, "0600", configJSON},
		{"службу", UnitPath, "0644", []byte(unitFile())},
		{"скрипт установки", ScriptPath, "0700", []byte(installScript(req.Services))},
	}
	// The extra files go up before the service is started, so the resolver
	// finds its address set the first time it runs.
	files = append(files, extra...)

	for _, f := range files {
		logf("  загружаю %s (%d КБ)", f.What, (len(f.Content)+1023)/1024)
		if err := cli.Upload(f.Path, f.Mode, f.Content); err != nil {
			return err
		}
	}

	out, err := cli.Run("sh " + ScriptPath)
	if err != nil {
		journal, _ := cli.Run("journalctl -u awgnode -n 40 --no-pager")
		return fmt.Errorf("установка не прошла: %w\n--- журнал ---\n%s", err, journal)
	}
	if out != "" {
		logf("  %s", out)
	}
	return nil
}

// waitStatus polls the node's status file until it appears.
func waitStatus(ctx context.Context, cli *remote.Client, path string, logf Logf) (node.Status, error) {
	deadline := time.Now().Add(StatusTimeout)
	for {
		raw, err := cli.Run("cat " + path + " 2>/dev/null || true")
		if err == nil && raw != "" {
			status, parseErr := node.ParseStatus([]byte(raw))
			if parseErr == nil {
				logf("узел поднялся: интерфейс %s, порт %d", status.Interface, status.ListenPort)
				return status, nil
			}
		}
		if time.Now().After(deadline) {
			journal, _ := cli.Run("journalctl -u awgnode -n 40 --no-pager")
			return node.Status{}, fmt.Errorf("узел не сообщил о себе за %s\n--- журнал ---\n%s", StatusTimeout, journal)
		}
		select {
		case <-ctx.Done():
			return node.Status{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// verifyListening checks that the node really holds the UDP port.
func verifyListening(cli *remote.Client, port int) error {
	out, err := cli.Run(fmt.Sprintf("ss -lun 2>/dev/null | grep -c ':%d' || true", port))
	if err != nil {
		return err
	}
	if out == "0" || out == "" {
		return fmt.Errorf("на сервере никто не слушает UDP %d", port)
	}
	return nil
}

// waitSplit re-reads the status until it carries the resolver's section. The
// first status can be written a moment before the resolver has finished
// starting.
func waitSplit(ctx context.Context, cli *remote.Client, cfg *node.Config, logf Logf) (node.Status, error) {
	deadline := time.Now().Add(StatusTimeout)
	for {
		raw, err := cli.Run("cat " + cfg.Status() + " 2>/dev/null || true")
		if err == nil && raw != "" {
			if status, parseErr := node.ParseStatus([]byte(raw)); parseErr == nil && status.Split != nil {
				return status, nil
			}
		}
		if time.Now().After(deadline) {
			journal, _ := cli.Run("journalctl -u awgnode -n 40 --no-pager")
			return node.Status{}, fmt.Errorf("резолвер не запустился за %s\n--- журнал ---\n%s", StatusTimeout, journal)
		}
		select {
		case <-ctx.Done():
			return node.Status{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// WaitStatus waits for the node to write its status after a (re)start. With
// split routing configured it also waits for the resolver's section, which
// appears a moment later.
func WaitStatus(ctx context.Context, cli *remote.Client, cfg *node.Config, logf Logf) (node.Status, error) {
	status, err := waitStatus(ctx, cli, cfg.Status(), logf)
	if err != nil {
		return node.Status{}, err
	}
	if cfg.Split != nil && status.Split == nil {
		return waitSplit(ctx, cli, cfg, logf)
	}
	return status, nil
}
