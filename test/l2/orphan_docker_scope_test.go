//go:build multiproc

package l2

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// orphanDockerScope uses the logical-node wrapper's label-filtered ps contract,
// but permits mutations only for this test's one container. The agent still
// calls the real Docker CLI and daemon; unrelated containers cannot be swept.
func orphanDockerScope(t *testing.T, realDocker, name, owner string) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
set -eu
real=%s
name=%s
owner=%s
case "$*" in
  'ps -a --format {{.Names}}'|'ps --format {{.Names}}')
    command="$1"; shift
    exec "$real" "$command" --filter "label=gpuworks.l2-orphan=$owner" "$@" ;;
  'stop -t 10 '"$name"|'rm -f '"$name")
    actual=$("$real" inspect --format '{{index .Config.Labels "gpuworks.l2-orphan"}}' "$name")
    [ "$actual" = "$owner" ] || { echo 'orphan fixture ownership mismatch' >&2; exit 93; }
    exec "$real" "$@" ;;
  info|info\ *|version|version\ *|images|images\ *|'image inspect '*|'image ls'|'image ls '*)
    exec "$real" "$@" ;;
  *) echo 'Docker command outside orphan fixture scope' >&2; exit 94 ;;
esac
`, orphanShellQuote(realDocker), orphanShellQuote(name), orphanShellQuote(owner))
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func orphanShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func TestOrphanDockerScopeRejectsForeignMutations(t *testing.T) {
	// This executable records exact argv; it never contacts a Docker daemon.
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	real := filepath.Join(dir, "real-docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + orphanShellQuote(log) + "\ncase \"$1\" in inspect) echo owner;; esac\n"
	if err := os.WriteFile(real, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(orphanDockerScope(t, real, "gpuworks-owned", "owner"), "docker")
	for _, args := range [][]string{{"rm", "-f", "gpuworks-foreign"}, {"stop", "-t", "10", "gpuworks-foreign"}, {"container", "prune", "-f"}, {"ps", "-a"}} {
		if err := exec.Command(wrapper, args...).Run(); err == nil {
			t.Fatalf("unscoped command accepted: %v", args)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("rejected command reached Docker")
	}
	for _, args := range [][]string{{"ps", "-a", "--format", "{{.Names}}"}, {"rm", "-f", "gpuworks-owned"}} {
		if out, err := exec.Command(wrapper, args...).CombinedOutput(); err != nil {
			t.Fatalf("scoped command %v: %v: %s", args, err, out)
		}
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "ps --filter label=gpuworks.l2-orphan=owner -a --format {{.Names}}\ninspect --format {{index .Config.Labels \"gpuworks.l2-orphan\"}} gpuworks-owned\nrm -f gpuworks-owned\n"
	if string(calls) != want {
		t.Fatalf("Docker calls=%q want=%q", calls, want)
	}

	// A matching name alone must never authorize deletion after a collision or
	// replacement; the daemon's current label must match as well.
	if err := os.WriteFile(real, []byte("#!/bin/sh\ncase \"$1\" in inspect) echo foreign;; *) exit 0;; esac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(wrapper, "rm", "-f", "gpuworks-owned").CombinedOutput(); err == nil || !strings.Contains(string(out), "ownership mismatch") {
		t.Fatalf("foreign label accepted: err=%v output=%s", err, out)
	}
}
