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

	"awgrotator/internal/deploy"
	"awgrotator/internal/enroll"
	"awgrotator/internal/globalping"
	"awgrotator/internal/node"
	"awgrotator/internal/splitplan"
	"awgrotator/internal/tunneltest"
	"awgrotator/internal/vultr"
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

// ---- cloud

// VultrCloud is the engine's view of the cloud: only what carries our tag.
//
// Listing is filtered by the provider, not afterwards, so an instance without
// the tag is not merely skipped but never seen. That is what makes it
// impossible for the manager to delete a server that belongs to someone else,
// or to the operator's other projects on the same account.
type VultrCloud struct {
	Client *vultr.Client
	Tag    string
}

// Tagged implements Cloud.
func (c *VultrCloud) Tagged(ctx context.Context) ([]vultr.Instance, error) {
	return c.Client.InstancesByTag(ctx, c.Tag)
}

// Delete implements Cloud. A server that is already gone counts as deleted:
// the aim is that it does not exist, and failing because someone got there
// first would only make a retry loop of it.
func (c *VultrCloud) Delete(ctx context.Context, id string) error {
	err := c.Client.DeleteInstance(ctx, id)
	if err != nil && strings.Contains(err.Error(), "404") {
		return nil
	}
	return err
}

// ---- provisioning

// DeployProvisioner builds a server with the same code the operator's tool
// uses, but takes the programs from this machine's own disk: a server has no
// compiler, and does not need one to copy itself.
type DeployProvisioner struct {
	Cfg    *Config
	Client *vultr.Client

	// NodeBinary and ManagerBinary are the paths of the programs to copy.
	// They default to where they are installed.
	NodeBinary    string
	ManagerBinary string

	Logf func(format string, args ...any)
}

// Provision implements Provisioner.
func (p *DeployProvisioner) Provision(ctx context.Context, req ProvisionRequest) (*deploy.Result, error) {
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
	return deploy.Provision(ctx, p.Client, deploy.Request{
		Region:    req.Region,
		Plan:      p.Cfg.Plan,
		OsID:      p.Cfg.OsID,
		Tag:       p.Cfg.Tag,
		Label:     req.Label,
		SSHKeyID:  p.Cfg.SSHKeyID,
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

// ---- reachability from outside

// GlobalpingProber measures reachability from probes in the target country and
// from a control group elsewhere.
type GlobalpingProber struct {
	Client *globalping.Client

	// Country is where the server needs to be reachable from, and Control a
	// country far from it. The defaults are Russia and Germany.
	Country string
	Control string
}

// Reach implements Prober. It uses ICMP, so it can see that an address has
// stopped answering from the target country but not that one protocol is
// being filtered while everything else still passes. The manager says so in
// its status rather than letting a healthy verdict suggest otherwise.
func (p *GlobalpingProber) Reach(ctx context.Context, ip string) (target, control Reach, err error) {
	gp := p.Client
	if gp == nil {
		gp = globalping.New()
	}

	t, err := gp.Ping(ctx, ip, orDefault(p.Country, "RU"), 10)
	if err != nil {
		return Reach{}, Reach{}, err
	}
	c, err := gp.Ping(ctx, ip, orDefault(p.Control, "DE"), 6)
	if err != nil {
		return Reach{}, Reach{}, err
	}
	return Reach{Reached: t.Reached, Total: t.Total}, Reach{Reached: c.Reached, Total: c.Total}, nil
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

// ReadRegistry implements Host.
func (h *SystemHost) ReadRegistry() ([]byte, error) {
	raw, err := os.ReadFile(enroll.DefaultRegistryPath)
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
