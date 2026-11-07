package deploy

import (
	"strings"
	"testing"
)

func TestInstallScriptStartsTheNodeFirst(t *testing.T) {
	script := installScript(nil)
	for _, want := range []string{
		"set -eu",
		"systemctl enable awgnode",
		"systemctl restart awgnode",
		"apt-get install -y -qq nftables",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script is missing %q:\n%s", want, script)
		}
	}
	// The nftables service must never be enabled: it would load the distro's
	// ruleset and flush the table the node installs.
	if strings.Contains(script, "enable nftables") || strings.Contains(script, "restart nftables") {
		t.Error("the script starts the nftables service, which would wipe the node's table")
	}
}

func TestInstallScriptStartsExtraServicesAfterTheNode(t *testing.T) {
	script := installScript([]string{"awgmgr", "other"})

	node := strings.Index(script, "systemctl restart awgnode")
	mgr := strings.Index(script, "systemctl restart awgmgr")
	other := strings.Index(script, "systemctl restart other")
	if node < 0 || mgr < 0 || other < 0 {
		t.Fatalf("a service is not started:\n%s", script)
	}
	if !(node < mgr && mgr < other) {
		t.Errorf("services are not started in order (node %d, awgmgr %d, other %d)", node, mgr, other)
	}
	if !strings.Contains(script, "systemctl enable awgmgr") {
		t.Error("an extra service is started but not enabled for the next boot")
	}
	if !strings.HasSuffix(script, "\n") {
		t.Error("the script does not end with a newline")
	}
}

func TestUnitFileRestartsOnFailure(t *testing.T) {
	unit := unitFile()
	for _, want := range []string{"Restart=always", "ExecStart=" + BinaryPath, "-config " + ConfigPath, "WantedBy=multi-user.target"} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit is missing %q:\n%s", want, unit)
		}
	}
}
