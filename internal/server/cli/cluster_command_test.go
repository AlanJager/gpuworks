package cli

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterCommandGroupHelpAndRejectionHaveNoSideEffects(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
	}{
		{"bare", nil, 2},
		{"help word", []string{"help"}, 0},
		{"help long", []string{"--help"}, 0},
		{"help short", []string{"-h"}, 0},
	}
	for _, command := range []string{"unknown", "upgrade", "update", "init", "join", "address", "status", "serve", "install-node", "check-node", "cluster-install"} {
		cases = append(cases, struct {
			name string
			args []string
			code int
		}{command, []string{command, "--config", "missing.yaml"}, 2})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertUnchanged := clusterCommandNoSideEffects(t)
			var output, logs bytes.Buffer
			code := run(slog.New(slog.NewTextHandler(&logs, nil)), append([]string{"cluster"}, tc.args...), &output)
			if code != tc.code || logs.Len() != 0 {
				t.Fatalf("exit=%d, want=%d, logs=%s", code, tc.code, logs.String())
			}
			for _, want := range []string{"usage:", "gpuworks cluster install", "gpuworks cluster check"} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("missing %q in group usage: %s", want, output.String())
				}
			}
			if tc.code == 2 && len(tc.args) > 0 && !strings.Contains(output.String(), "unknown cluster subcommand "+`"`+tc.args[0]+`"`) {
				t.Fatalf("missing explicit rejection: %s", output.String())
			}
			for _, unsupported := range []string{"gpuworks cluster init", "gpuworks cluster upgrade", "gpuworks cluster update", "本地安装日志", "安装进度"} {
				if strings.Contains(output.String(), unsupported) {
					t.Fatalf("group output advertised or started an unsupported action: %s", output.String())
				}
			}
			assertUnchanged()
		})
	}
}

func TestClusterCommandHelpAndLegacyAliasesHaveNoSideEffects(t *testing.T) {
	for _, command := range []string{"install", "check"} {
		for _, prefix := range [][]string{{"cluster", command}, {"cluster-" + command}} {
			for _, help := range []string{"-h", "--help"} {
				t.Run(strings.Join(prefix, " ")+"/"+help, func(t *testing.T) {
					assertUnchanged := clusterCommandNoSideEffects(t)
					// These paths must not be read while printing help.
					args := append(append([]string{}, prefix...), "--config", "missing.yaml", "--password-file", "missing.password", help)
					var output, logs bytes.Buffer
					code := run(slog.New(slog.NewTextHandler(&logs, nil)), args, &output)
					if code != 0 || logs.Len() != 0 {
						t.Fatalf("help exit=%d, logs=%s", code, logs.String())
					}
					if !strings.HasPrefix(output.String(), "Usage of gpuworks cluster "+command+":\n") || !strings.Contains(output.String(), "-binary") {
						t.Fatalf("missing canonical command help: %s", output.String())
					}
					if strings.Contains(output.String(), "本地安装日志") || strings.Contains(output.String(), "安装进度") {
						t.Fatal("help started an installation transcript")
					}
					assertUnchanged()
				})
			}
		}
	}
}

func TestClusterCommandAndLegacyAliasesUseTheSameValidation(t *testing.T) {
	for _, command := range []string{"install", "check"} {
		t.Run(command, func(t *testing.T) {
			assertUnchanged := clusterCommandNoSideEffects(t)
			config := writeClusterConfig(t, clusterPasswordPromptConfig)
			var prior string
			for _, prefix := range [][]string{{"cluster", command}, {"cluster-" + command}} {
				args := append(append([]string{}, prefix...), "--config", config, "--password-file", "missing.password")
				var output, logs bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
					if attr.Key == slog.TimeKey {
						return slog.Attr{}
					}
					return attr
				}}))
				if code := run(logger, args, &output); code != 1 {
					t.Fatalf("validation exit=%d, logs=%s", code, logs.String())
				}
				want := "--binary is required"
				if command == "check" {
					want = "gpuworks cluster check does not use --password-file"
				}
				if !strings.Contains(logs.String(), want) || output.Len() != 0 {
					t.Fatalf("wrong command validation: output=%s, logs=%s", output.String(), logs.String())
				}
				if prior != "" && prior != logs.String() {
					t.Fatalf("alias validation differs: grouped=%s, alias=%s", prior, logs.String())
				}
				prior = logs.String()
			}
			assertUnchanged()
		})
	}
}

func clusterCommandNoSideEffects(t *testing.T) func() {
	t.Helper()
	sshLog := clusterPasswordPromptSSHFailure(t)
	work := t.TempDir()
	t.Chdir(work)
	return func() {
		t.Helper()
		if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
			t.Fatalf("command attempted SSH: %v", err)
		}
		entries, err := os.ReadDir(work)
		if err != nil || len(entries) != 0 {
			t.Fatalf("command changed working directory %s: entries=%v err=%v", filepath.Base(work), entries, err)
		}
	}
}

func run(log *slog.Logger, args []string, output io.Writer) int {
	return Run(log, args, output, Options{})
}
