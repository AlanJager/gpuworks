package udocker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/runveil"
)

// An agent started from /root (mode 700) used to hand that cwd to a PRoot
// running as the dedicated user, which then failed with "can't chdir to
// '/root'". Every process the agent starts for the runtime must run in a
// directory that does not depend on where the agent was started.
func TestRunveilExecDoesNotInheritTheAgentWorkingDirectory(t *testing.T) {
	d := runveilDriver(t)
	command := &runveil.Command{Argv: []string{"sh", "-c", "readlink /proc/self/cwd"}}
	for _, ctx := range []context.Context{nil, context.Background()} {
		if cmd := d.runveilExec(ctx, command, nil); cmd.Dir != "/" {
			t.Errorf("runveilExec Dir = %q, want \"/\"", cmd.Dir)
		}
	}

	// And for real: a child of an agent whose cwd was removed under it.
	gone := filepath.Join(t.TempDir(), "agent-cwd")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	out, err := d.runveilExec(context.Background(), command, nil).Output()
	if err != nil {
		t.Fatalf("run from a vanished cwd: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "/" {
		t.Fatalf("child cwd = %q, want \"/\"", got)
	}
}

func TestUdockerCLIPullDoesNotInheritTheAgentWorkingDirectory(t *testing.T) {
	d := runveilDriver(t)
	if cmd := d.cliPullCommand(context.Background(), "example/image:tag"); cmd.Dir != "/" {
		t.Errorf("cliPullCommand Dir = %q, want \"/\"", cmd.Dir)
	}
}
