package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"awgrotator/internal/cloud"
	"awgrotator/internal/deploy"
	"awgrotator/internal/egress"
	"awgrotator/internal/enroll"
	"awgrotator/internal/node"
	"awgrotator/internal/splitplan"
	"awgrotator/internal/tunneltest"
)

// Where the manager's program and service live on a server.
const (
	ManagerBinaryPath = "/usr/local/bin/awgmgr"
	ManagerUnitPath   = "/etc/systemd/system/awgmgr.service"
	ManagerService    = "awgmgr"
)

// ManagerUnit is the systemd unit that runs the manager. It starts after the
// node and is restarted whatever happens: a manager that has stopped is a
// server that has stopped looking after itself.
func ManagerUnit() string {
	return fmt.Sprintf(`[Unit]
Description=AmneziaWG manager
After=network-online.target awgnode.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s -config %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, ManagerBinaryPath, DefaultConfigPath)
}

// ---- provisioning

// DeployProvisioner builds a server with the same code the operator's tool
// uses, but takes the programs from this machine's own disk: a server has no
// compiler, and does not need one to copy itself.
type DeployProvisioner struct {
	Cfg    *Config
	Clouds cloud.Set

	// NodeBinary and ManagerBinary are the paths of the programs to copy.
	// They default to where they are installed.
	NodeBinary    string
	ManagerBinary string

	Logf func(format string, args ...any)
}

// Provision implements Provisioner.
func (p *DeployProvisioner) Provision(ctx context.Context, req ProvisionRequest) (*deploy.Result, error) {
	provider, err := p.Clouds.Get(req.Place.Provider)
	if err != nil {
		return nil, err
	}
	settings := p.Cfg.Clouds[req.Place.Provider]
	signer, err := p.Cfg.SSHSigner()
	if err != nil {
		return nil, err
	}
	nodeBin, err := os.ReadFile(orDefault(p.NodeBinary, deploy.BinaryPath))
	if err != nil {
		return nil, fmt.Errorf("read the node program to copy: %w", err)
	}
	mgrBin, err := os.ReadFile(orDefault(p.ManagerBinary, ManagerBinaryPath))
	if err != nil {
		return nil, fmt.Errorf("read the manager program to copy: %w", err)
	}

	files := append([]deploy.File(nil), req.Files...)
	files = append(files,
		deploy.File{What: "менеджер", Path: ManagerBinaryPath, Mode: "0700", Content: mgrBin},
		deploy.File{What: "служба менеджера", Path: ManagerUnitPath, Mode: "0644", Content: []byte(ManagerUnit())},
	)

	logf := p.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return deploy.Provision(ctx, provider, deploy.Request{
		Location:  req.Place.Location,
		Billing:   req.Billing,
		Label:     req.Label,
		SSHKeyID:  settings.SSHKeyID,
		Binary:    nodeBin,
		Config:    req.Node,
		Files:     files,
		FilesFor:  req.FilesFor,
		OnCreated: req.OnCreated,
		Services:  []string{ManagerService},
		Signer:    signer,
	}, logf)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// ---- the tunnel test

// TunnelTester runs the in-memory client against a server.
type TunnelTester struct{}

// Test implements Tester.
func (TunnelTester) Test(ctx context.Context, target tunneltest.Target) (tunneltest.Report, error) {
	return tunneltest.Run(ctx, target, tunneltest.Options{})
}

// ---- this machine

// StatusMaxAge is how old the node's status may be before it is taken to mean
// the node has stopped. The node writes it every ten seconds.
const StatusMaxAge = 45 * time.Second

// SystemHost is the real machine: systemd for the service, files for the rest.
type SystemHost struct {
	Cfg *Config

	// StatusPath overrides where the node's status is read from.
	StatusPath string

	// ExitWantPath overrides where the node is told which way out to use.
	ExitWantPath string
}

// WriteExitWant implements Host.
func (h *SystemHost) WriteExitWant(m egress.Mode) error {
	return egress.WriteWant(orDefault(h.ExitWantPath, egress.DefaultWantPath), m)
}

func (h *SystemHost) statusPath() string { return orDefault(h.StatusPath, node.DefaultStatusPath) }

// NodeRunning implements Host.
func (h *SystemHost) NodeRunning(ctx context.Context) (bool, error) {
	err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", "awgnode").Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil // systemctl ran and said "not active"
	}
	return false, err
}

// RestartNode implements Host.
func (h *SystemHost) RestartNode(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "systemctl", "restart", "awgnode").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart awgnode: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// NodeStatus implements Host.
func (h *SystemHost) NodeStatus(context.Context) (node.Status, error) {
	raw, err := os.ReadFile(h.statusPath())
	if err != nil {
		return node.Status{}, fmt.Errorf("the node has not written a status: %w", err)
	}
	status, err := node.ParseStatus(raw)
	if err != nil {
		return node.Status{}, err
	}
	if age := time.Since(status.UpdatedAt); age > StatusMaxAge {
		return node.Status{}, fmt.Errorf("the node's status is %s old: it has stopped updating", age.Round(time.Second))
	}
	return status, nil
}

// ReadPool implements Host.
func (h *SystemHost) ReadPool() ([]byte, error) {
	raw, err := os.ReadFile(node.DefaultPoolPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

// ReadDirect implements Host.
func (h *SystemHost) ReadDirect() ([]byte, error) {
	raw, err := os.ReadFile(node.DefaultDirectPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

// ReadWatchLog implements Host: the end of the availability log, for a
// replacement to carry on from.
func (h *SystemHost) ReadWatchLog() ([]byte, error) {
	l := &WatchLog{Path: filepath.Join(h.Cfg.Data(), WatchLogName)}
	return l.Tail(InheritedWatchBytes)
}

// ReadUsage implements Host.
func (h *SystemHost) ReadUsage() ([]byte, error) {
	raw, err := os.ReadFile(node.DefaultUsagePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

// Usage implements Host, through the node's control socket.
func (h *SystemHost) Usage(ctx context.Context, period string) (*node.UsageView, error) {
	resp, err := node.Control(ctx, node.DefaultControlPath, node.ControlRequest{Op: node.OpUsage, Period: period})
	if err != nil {
		return nil, err
	}
	if !resp.OK || resp.Usage == nil {
		return nil, errors.New(resp.Error)
	}
	return resp.Usage, nil
}

// ReadRegistry implements Host.
func (h *SystemHost) ReadRegistry() ([]byte, error) {
	raw, err := os.ReadFile(enroll.DefaultRegistryPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

// ReadAccess implements Host.
func (h *SystemHost) ReadAccess() ([]byte, error) {
	raw, err := os.ReadFile(enroll.DefaultAccessPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

func (h *SystemHost) bodyPath() string { return filepath.Join(h.Cfg.Data(), "body.json") }

// ReadBody implements Host.
func (h *SystemHost) ReadBody() ([]byte, error) {
	raw, err := os.ReadFile(h.bodyPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

// WriteBody implements Host.
func (h *SystemHost) WriteBody(body []byte) error {
	if err := os.MkdirAll(filepath.Dir(h.bodyPath()), 0o700); err != nil {
		return err
	}
	tmp := h.bodyPath() + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.bodyPath())
}

// ---- preparing split routing for a successor

// PlanFunc returns the function the engine uses to prepare split routing for a
// new server. It refreshes the address set from RIPE, which is what keeps it
// current: every new server starts with a fresh one.
func PlanFunc(cfg *Config, logf func(string, ...any)) func(context.Context) (*splitplan.Plan, error) {
	return func(ctx context.Context) (*splitplan.Plan, error) {
		return splitplan.Prepare(ctx, splitplan.Sources{
			CacheDir: cfg.Data(),
			RulesDir: DefaultRulesDir,
		}, logf)
	}
}
