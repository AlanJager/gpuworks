package gpu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIsOnHost(t *testing.T) {
	root := t.TempDir()
	for path, mode := range map[string]os.FileMode{
		"/usr/local/bin/ppu-smi":                   0o755,
		"/usr/local/Ascend/driver/tools/hccn_tool": 0o755,
		"/usr/bin/not-executable":                  0o644,
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	// Hygon's hy-smi is an absolute link into the driver's tree, and the
	// tree's own link is relative.
	for link, target := range map[string]string{
		"/usr/local/bin/hy-smi":       "/usr/local/hyhal/bin/hy-smi",
		"/usr/local/hyhal/bin/hy-smi": "hy-smi.real",
		"/usr/bin/loop-a":             "/usr/bin/loop-b",
		"/usr/bin/loop-b":             "/usr/bin/loop-a",
	} {
		full := filepath.Join(root, link)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, full); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "/usr/local/hyhal/bin/hy-smi.real"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	oldRoot := hostRoot
	hostRoot = root
	cliOnHost.Store(true)
	t.Cleanup(func() { hostRoot = oldRoot; cliOnHost.Store(false) })

	if p, err := lookCLI("ppu-smi"); err != nil || p != "/usr/local/bin/ppu-smi" {
		t.Fatalf("lookCLI(ppu-smi) = %q, %v", p, err)
	}
	if p, err := lookCLI("hccn_tool"); err != nil || p != "/usr/local/Ascend/driver/tools/hccn_tool" {
		t.Fatalf("lookCLI(hccn_tool) = %q, %v", p, err)
	}
	if p, err := lookCLI("hy-smi"); err != nil || p != "/usr/local/bin/hy-smi" {
		t.Fatalf("lookCLI(hy-smi) through host links = %q, %v", p, err)
	}
	// An absolute path (DTK's /opt/dtk/bin/hy-smi, ldconfig found earlier)
	// is looked up on the host as well, not in the agent's own image.
	if p, err := lookCLI("/usr/local/bin/ppu-smi"); err != nil || p != "/usr/local/bin/ppu-smi" {
		t.Fatalf("lookCLI(absolute host path) = %q, %v", p, err)
	}
	for _, missing := range []string{"npu-smi", "not-executable", "loop-a", "/usr/bin/not-executable", "/opt/dtk/bin/hy-smi"} {
		if _, err := lookCLI(missing); !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("lookCLI(%s) = %v", missing, err)
		}
	}
	cmd := cliCommand(context.Background(), "ppu-smi", "-L")
	if got := strings.Join(cmd.Args, " "); got != "nsenter -t 1 -m -- /usr/local/bin/ppu-smi -L" {
		t.Fatalf("command = %s", got)
	}
	cliOnHost.Store(false)
	if got := strings.Join(cliCommand(context.Background(), "ppu-smi", "-L").Args, " "); got != "ppu-smi -L" {
		t.Fatalf("command off the host = %s", got)
	}
}
