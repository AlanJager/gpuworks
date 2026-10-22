package runveilcli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The argv drops to the runtime user before PRoot starts; that user may not be
// able to enter the caller's cwd (/root), so the command must not inherit it.
func TestRunCommandDoesNotInheritTheCallersWorkingDirectory(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "caller-cwd")
	must(os.Mkdir(gone, 0o755))
	t.Chdir(gone)
	must(os.Remove(gone))

	var out bytes.Buffer
	c := &CLI{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &bytes.Buffer{}}
	code, err := c.RunCommand(context.Background(), []string{"sh", "-c", "readlink /proc/self/cwd"})
	if err != nil || code != 0 {
		t.Fatalf("RunCommand = %d, %v", code, err)
	}
	if got := strings.TrimSpace(out.String()); got != "/" {
		t.Fatalf("command cwd = %q, want \"/\"", got)
	}
}
