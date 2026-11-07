package lifecycle

import (
	"os"
	"path/filepath"

	"awgrotator/internal/awg"
	"awgrotator/internal/deploy"
	"awgrotator/internal/node"
	"awgrotator/internal/vultr"
)

type deployFile = deploy.File

func mkdir(path string) error { return os.MkdirAll(path, 0o755) }

func writeText(path, text string) error { return os.WriteFile(path, []byte(text), 0o600) }

func vultrInstance(id string) vultr.Instance {
	return vultr.Instance{ID: id, MainIP: "192.0.2.77", Region: "ams", Tags: []string{"awg-rotator"}}
}

// DataPath is where a manager keeps a file of its own.
func DataPath(c *Config, name string) string { return filepath.Join(c.Data(), name) }

var _ = awg.MTU
var _ = node.ConfigVersion
