package selfinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func TestSharedRootUpgradeRestoresMissingPrivateConfigBeforeOldServiceHealth(t *testing.T) {
	root, agent := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	original := readFile(t, h.layout.ConfigPath()) + "\n# retain operator policy on rollback\nmetrics_retention: 168h\n"
	writeFile(t, h.layout.ConfigPath(), original, 0o664)
	private := filepath.Join(root, "server-config", "server.yaml")
	alias := filepath.Join(root, "server.yaml")
	// Compare only identity/mode: adding server-owned children legitimately
	// changes the shared directory's timestamps, but never its authority.
	permissions := func(path string) string {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("fixture stat has no ownership")
		}
		return fmt.Sprintf("%v:%d:%d", info.Mode(), owner.Uid, owner.Gid)
	}
	before := map[string]string{root: permissions(root)}
	for _, path := range agent {
		for at := path; at != root; at = filepath.Dir(at) {
			before[at] = permissions(at)
		}
	}
	h.useSite("v2.0.0", []byte("new binary"))
	loadedConfig := alias
	runningBinary := ""
	runningConfig := ""
	var starts []string
	h.run.on = func(cmd Command) {
		if cmd.String() == "sudo systemctl daemon-reload" && h.run.failure(cmd) == nil {
			if strings.Contains(readFile(t, h.layout.UnitPath()), "--config "+private+"\n") {
				loadedConfig = private
			}
		}
		if cmd.String() == "sudo systemctl restart gpuworks" {
			runningBinary = readFile(t, h.layout.Binary())
			runningConfig = loadedConfig
			starts = append(starts, runningConfig)
		}
	}
	removed := false
	healthyOld := 0
	h.installer.deps.Health = func(_ context.Context, _ string) (string, error) {
		switch runningBinary {
		case "new binary":
			if !removed {
				if runningConfig != private {
					return "", fmt.Errorf("new service used %q instead of private config", runningConfig)
				}
				if err := os.Remove(private); err != nil {
					return "", fmt.Errorf("remove private config during unhealthy startup: %w", err)
				}
				removed = true
			}
			return "", errors.New("new version removed its configuration and failed startup")
		case "old binary":
			if runningConfig != private {
				return "", fmt.Errorf("old service used %q instead of private config", runningConfig)
			}
			raw, err := os.ReadFile(runningConfig)
			if err != nil {
				return "", fmt.Errorf("old service cannot read private config: %w", err)
			}
			if !bytes.Equal(raw, []byte(original)) {
				return "", errors.New("old service received changed configuration")
			}
			if _, err := serverconfig.Load(runningConfig); err != nil {
				return "", fmt.Errorf("old service cannot parse restored config: %w", err)
			}
			healthyOld++
			return "v1.0.0", nil
		default:
			return "", fmt.Errorf("health before a fixture service restart: %q", runningBinary)
		}
	}
	report, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil || !report.RolledBack || !removed {
		t.Fatalf("missing failed-upgrade rollback: report=%+v err=%v removed=%v", report, err, removed)
	}
	if got, readErr := os.ReadFile(private); readErr != nil || string(got) != original {
		t.Errorf("backup did not restore original private config: bytes=%q err=%v", got, readErr)
	}
	if target, linkErr := os.Readlink(alias); linkErr != nil || target != "server-config/server.yaml" {
		t.Errorf("rollback lost canonical compatibility alias: target=%q err=%v", target, linkErr)
	}
	if len(starts) != 2 || starts[0] != private || starts[1] != private || healthyOld != 1 {
		t.Errorf("old service did not become healthy from its private config: starts=%v healthyOld=%d", starts, healthyOld)
	}
	if readFile(t, h.layout.Binary()) != "old binary" {
		t.Error("rollback did not restore old executable")
	}
	if got := readArchive(t, report.Backup)["server.yaml"]; got != original {
		t.Errorf("backup omitted original config: %q", got)
	}
	for path, want := range before {
		if got := permissions(path); got != want {
			t.Errorf("rollback changed shared/agent authority of %s: %s want %s", path, got, want)
		}
	}
	for _, path := range agent {
		if readFile(t, path) != "agent state" {
			t.Errorf("rollback changed agent content %s", path)
		}
	}
	for _, cmd := range h.run.calls {
		if cmd.Path != "chown" && cmd.Path != "chmod" && cmd.Path != "find" {
			continue
		}
		target := cmd.Args[len(cmd.Args)-1]
		if cmd.Path == "find" {
			target = cmd.Args[0]
		}
		if target == root {
			t.Errorf("rollback handed off shared root: %s", cmd)
		}
		for _, path := range agent {
			if pathWithin(target, path) {
				t.Errorf("rollback permission command reaches agent state %s: %s", path, cmd)
			}
		}
	}
}
