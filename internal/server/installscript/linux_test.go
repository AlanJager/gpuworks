package installscript

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A Linux node whose uid has no name (a container started with an arbitrary
// uid, as the CI runner is) installs: only the Mac path asks for a user name,
// and `id -un` fails there with "cannot find name for user ID".
func TestScriptOnLinuxNeedsNoUserName(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	for _, tool := range []string{"sh", "cat", "cp", "sed", "mkdir", "rm", "chmod", "mv", "printf", "echo", "nohup", "tr", "awk"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		if err := os.Symlink(real, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("uname", `case "$1" in -m) echo x86_64 ;; -s) echo Linux ;; esac`)
	stub("id", `case "$1" in -u) echo 998 ;; -g) echo 998 ;; *) echo "id: cannot find name for user ID 998" >&2; exit 1 ;; esac`)
	stub("curl", `while [ $# -gt 0 ]; do [ "$1" = "-o" ] && printf '#!/bin/sh\nexit 0\n' > "$2"; shift; done`)
	installDir := filepath.Join(t.TempDir(), "agent")
	cmd := exec.Command(filepath.Join(dir, "sh"), "-c", Script, "install.sh", "--server", "http://gw:8800", "--token", "t", "--install-dir", installDir)
	cmd.Env = []string{"PATH=" + dir, "HOME=" + home, "GPUWORKS_NO_SYSTEMD=1"}
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "cannot find name") {
		t.Fatalf("install = %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(installDir, "gpuworks-agent")); err != nil {
		t.Fatalf("agent not installed: %v\n%s", err, out)
	}
}

// --no-systemd runs the agent under nohup and leaves a gpuworks-agent unit on
// the host alone, so one host can run several agents; only --clean-existing
// as well retires the unit. GPUWORKS_NO_SYSTEMD=1 (hermetic installers) never
// touches it.
func TestScriptNoSystemdRetiresTheUnitOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		env    string
		retire bool
	}{
		{name: "no-systemd", args: []string{"--no-systemd"}},
		{name: "clean-existing", args: []string{"--no-systemd", "--clean-existing"}, retire: true},
		{name: "env", env: "GPUWORKS_NO_SYSTEMD=1"},
		{name: "env-clean-existing", args: []string{"--clean-existing"}, env: "GPUWORKS_NO_SYSTEMD=1", retire: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, home := t.TempDir(), t.TempDir()
			for _, tool := range []string{"sh", "cat", "cp", "sed", "mkdir", "chmod", "mv", "printf", "echo", "nohup", "tr", "awk"} {
				real, err := exec.LookPath(tool)
				if err != nil {
					t.Skipf("no %s", tool)
				}
				if err := os.Symlink(real, filepath.Join(dir, tool)); err != nil {
					t.Fatal(err)
				}
			}
			realRM, err := exec.LookPath("rm")
			if err != nil {
				t.Skip("no rm")
			}
			calls := filepath.Join(t.TempDir(), "calls.log")
			stub := func(name, body string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub("uname", `case "$1" in -m) echo x86_64 ;; -s) echo Linux ;; esac`)
			stub("id", `case "$1" in -u|-g) echo 0 ;; esac`)
			stub("curl", `while [ $# -gt 0 ]; do [ "$1" = "-o" ] && printf '#!/bin/sh\nexit 0\n' > "$2"; shift; done`)
			stub("systemctl", `echo "systemctl $*" >> "$CALLS"`)
			stub("rm", `echo "rm $*" >> "$CALLS"
for f; do case "$f" in /etc/*) ;; *) "$REAL_RM" "$f" 2>/dev/null || true ;; esac; done`)
			installDir := filepath.Join(t.TempDir(), "agent")
			args := append([]string{"-c", Script, "install.sh", "--server", "http://gw:8800", "--token", "t", "--install-dir", installDir}, tc.args...)
			cmd := exec.Command(filepath.Join(dir, "sh"), args...)
			cmd.Env = []string{"PATH=" + dir, "HOME=" + home, "CALLS=" + calls, "REAL_RM=" + realRM}
			if tc.env != "" {
				cmd.Env = append(cmd.Env, tc.env)
			}
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "starting agent in background") {
				t.Fatalf("install = %v\n%s", err, out)
			}
			log, _ := os.ReadFile(calls)
			retired := strings.Contains(string(log), "systemctl disable --now gpuworks-agent") &&
				strings.Contains(string(log), "rm -f /etc/systemd/system/gpuworks-agent.service")
			if !tc.retire && strings.Contains(string(log), "systemctl") {
				t.Fatalf("install touched systemd without --clean-existing:\n%s", log)
			}
			if retired != tc.retire {
				t.Fatalf("unit retired = %v, want %v\n%s", retired, tc.retire, log)
			}
			if strings.Contains(string(log), "systemctl enable") {
				t.Fatalf("no-systemd install enabled the unit:\n%s", log)
			}
			if _, err := os.Stat(filepath.Join(installDir, "agent-restart.sh")); err != nil {
				t.Fatalf("nohup install left no restart script: %v", err)
			}
		})
	}
}
