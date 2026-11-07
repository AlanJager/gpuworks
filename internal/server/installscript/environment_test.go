package installscript

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScriptRunsRecordedConfigurationWithoutInheritedOverrides(t *testing.T) {
	h := newLinuxHost(t)
	captured := filepath.Join(t.TempDir(), "launch-environment")
	stub := strings.TrimSuffix(linuxAgentStub, "exit 0\n") + `if [ "$1" = run ]; then
  printf '%s\n' "${GPUWORKS_SERVER-unset}" "${GPUWORKS_TOKEN-unset}" "${GPUWORKS_CONNECT_MODE-unset}" "${GPUWORKS_SERVER_DIAL_LISTEN-unset}" "${GPUWORKS_RUNTIME_DRIVER-unset}" "${GPUWORKS_RUNTIME_MOUNT_ROOT-unset}" "${GPUWORKS_UDOCKER_BUNDLE-unset}" "${GPUWORKS_TEST_KEEP-unset}" > "$CAPTURED"
fi
exit 0
`
	if err := os.WriteFile(h.uploaded, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := []string{
		"CAPTURED=" + captured, "GPUWORKS_TEST_KEEP=kept",
		"GPUWORKS_SERVER=http://old.example", "GPUWORKS_TOKEN=old-token",
		"GPUWORKS_CONNECT_MODE=agent_dial", "GPUWORKS_SERVER_DIAL_LISTEN=127.0.0.1:1",
		"GPUWORKS_RUNTIME_DRIVER=udocker", "GPUWORKS_RUNTIME_MOUNT_ROOT=/old-mounts",
		"GPUWORKS_UDOCKER_BUNDLE=/old-bundle",
	}
	assertEnvironment := func() {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			data, _ := os.ReadFile(captured)
			if strings.Count(string(data), "\n") == 8 {
				want := strings.Repeat("unset\n", 7) + "kept\n"
				if string(data) != want {
					t.Fatalf("agent inherited stale launch overrides: %q", data)
				}
				return
			}
			select {
			case <-deadline.C:
				t.Fatal("installed agent did not record its environment")
			case <-tick.C:
			}
		}
	}
	if out, err := h.run(stale, "--token", "new-token", "--connect-mode", "server_dial", "--driver", "none", "--supervisor", "nohup"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	assertEnvironment()
	if err := os.Remove(captured); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(h.bin, "sh"), filepath.Join(h.stateDir, "agent-restart.sh"))
	command.Env = append([]string{"PATH=" + h.bin, "CALLS=" + h.calls}, stale...)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	assertEnvironment()
}
