package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Execute main in a child process so exit codes, flag.ExitOnError, and panics
// are tested through the same entry point as the installed binary.
func TestAgentCLI(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_AGENT_CLI") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"gpuworks-agent"}, os.Args[i+1:]...)
				break
			}
		}
		main()
		return
	}
	t.Setenv("GPUWORKS_SERVER", "")
	t.Setenv("GPUWORKS_TOKEN", "")
	// Never read the configuration of an agent installed on this machine.
	t.Setenv("GPUWORKS_STATE_DIR", t.TempDir())
	cases := []struct {
		name   string
		args   []string
		code   int
		output string
	}{
		{"empty", nil, 2, "usage:"},
		{"help", []string{"help"}, 0, "usage:"},
		{"short-help", []string{"-h"}, 0, "usage:"},
		{"long-help", []string{"--help"}, 0, "usage:"},
		{"version", []string{"version"}, 0, "gpuworks-agent"},
		{"version-flag", []string{"--version"}, 0, "gpuworks-agent"},
		{"doctor-positional", []string{"doctor", "unexpected"}, 2, "unexpected argument"},
		{"unknown-command", []string{"invalid-command"}, 2, "unknown subcommand"},
		{"bare-flag", []string{"--server", "http://localhost"}, 2, "unknown subcommand"},
		{"run-missing-server", []string{"run", "--token", "tok"}, 1, "server URL is required"},
		{"run-missing-token", []string{"run", "--server", "http://localhost"}, 1, "token is required"},
		{"run-server-dial-needs-no-server", []string{"run", "--token", "tok", "--connect-mode", "server_dial", "--listen", "0.0.0.0:99999"}, 1, "listen address"},
		{"upgrade-server-dial", []string{"upgrade", "--token", "tok", "--connect-mode", "server_dial"}, 1, "pushes upgrades"},
		{"configure-missing-state", []string{"configure"}, 1, "--state is required"},
		{"run-unknown-flag", []string{"run", "--invalid-flag"}, 2, "flag provided but not defined"},
		{"run-missing-value", []string{"run", "--server"}, 2, "flag needs an argument"},
	}
	for _, sub := range []string{"run", "smoke", "upgrade", "doctor", "grant-device-access", "bench", "install-service", "uninstall", "configure"} {
		cases = append(cases, struct {
			name   string
			args   []string
			code   int
			output string
		}{sub + "-help", []string{sub, "--help"}, 0, "Usage of "})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestAgentCLI$", "--"}, tc.args...)...)
			cmd.Env = append(os.Environ(), "GPUWORKS_TEST_AGENT_CLI=1")
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					code = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != tc.code || !strings.Contains(string(out), tc.output) || strings.Contains(string(out), "panic:") || strings.Contains(string(out), "flag: help requested") {
				t.Fatalf("exit=%d, want %d, output must contain %q without panic/help error:\n%s", code, tc.code, tc.output, out)
			}
		})
	}
}
