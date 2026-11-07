package lifecycle

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"awgrotator/internal/node"
)

func TestTheManagerUnitRestartsAndWaitsForTheNode(t *testing.T) {
	unit := ManagerUnit()
	for _, want := range []string{
		"After=network-online.target awgnode.service",
		"Restart=always",
		"ExecStart=" + ManagerBinaryPath + " -config " + DefaultConfigPath,
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the unit is missing %q:\n%s", want, unit)
		}
	}
}

func writeStatus(t *testing.T, path string, updated time.Time) {
	t.Helper()
	raw, err := json.Marshal(node.Status{Interface: "awg0", ListenPort: 1234, UpdatedAt: updated})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeText(path, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestNodeStatusIsTrustedOnlyWhileItIsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	h := &SystemHost{Cfg: testConfig(t, newTestKeys(t)), StatusPath: path}

	if _, err := h.NodeStatus(context.Background()); err == nil {
		t.Error("a missing status was accepted")
	}

	writeStatus(t, path, time.Now())
	st, err := h.NodeStatus(context.Background())
	if err != nil {
		t.Fatalf("a fresh status was refused: %v", err)
	}
	if st.ListenPort != 1234 {
		t.Errorf("status = %+v", st)
	}

	// A status that has stopped updating means the node has stopped, even
	// though the file is still there and still parses.
	writeStatus(t, path, time.Now().Add(-5*time.Minute))
	if _, err := h.NodeStatus(context.Background()); err == nil {
		t.Error("a stale status was accepted: a dead node would look alive")
	}

	if err := writeText(path, "not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.NodeStatus(context.Background()); err == nil {
		t.Error("a corrupt status was accepted")
	}
}

func TestBodyIsSavedAndReadBack(t *testing.T) {
	cfg := testConfig(t, newTestKeys(t))
	cfg.DataDir = t.TempDir()
	h := &SystemHost{Cfg: cfg}

	if got, err := h.ReadBody(); err != nil || got != nil {
		t.Fatalf("a missing body should read as empty, got %q / %v", got, err)
	}
	if err := h.WriteBody([]byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	got, err := h.ReadBody()
	if err != nil || string(got) != `{"x":1}` {
		t.Errorf("ReadBody = %q, %v", got, err)
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault("", "d") != "d" || orDefault("v", "d") != "v" {
		t.Error("orDefault is wrong")
	}
}
