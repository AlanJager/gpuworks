package docker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestStatusAndLogsParseFakeDockerCLI(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	script := strings.Join([]string{
		"#!/bin/sh",
		"case \"$1\" in",
		"inspect)",
		"  if [ \"$4\" = missing ]; then printf '%s\\n' 'error: no such object: missing' >&2; exit 1; fi",
		"  printf '%s' \"$DOCKER_INSPECT_OUTPUT\"",
		"  ;;",
		"logs)",
		"  printf '%s\\n' \"$@\" > \"$DOCKER_ARGS_FILE\"",
		"  printf 'INFO startup complete\\nERROR out of memory\\n'",
		"  ;;",
		"esac",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("DOCKER_ARGS_FILE", argsFile)
	d := New()
	for _, tc := range []struct {
		name string
		out  string
		want runtime.Status
	}{
		{name: "running", out: "true|0|false|healthy", want: runtime.Status{Exists: true, Running: true, Health: "healthy"}},
		{name: "exited", out: "false|17|false|", want: runtime.Status{Exists: true, ExitCode: 17}},
		{name: "oom", out: "false|137|true|unhealthy", want: runtime.Status{Exists: true, ExitCode: 137, OOMKilled: true, Health: "unhealthy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_INSPECT_OUTPUT", tc.out)
			got, err := d.Status(context.Background(), "container")
			if err != nil || got != tc.want {
				t.Fatalf("Status = %+v, err=%v; want %+v", got, err, tc.want)
			}
		})
	}
	t.Setenv("DOCKER_INSPECT_OUTPUT", "error: no such object: missing")
	if got, err := d.Status(context.Background(), "missing"); err != nil || got.Exists {
		t.Fatalf("missing Status = %+v, err=%v", got, err)
	}
	// The CLI's warnings share the combined output; the template result is
	// its last line.
	t.Setenv("DOCKER_INSPECT_OUTPUT", "WARNING: daemon config reload pending\nfalse|0|false|")
	if got, err := d.Status(context.Background(), "container"); err != nil || got != (runtime.Status{Exists: true}) {
		t.Fatalf("Status after a warning = %+v, err=%v", got, err)
	}
	// Output that does not parse must not read as a stopped container: the
	// engine retires a running instance on exactly that answer.
	for _, out := range []string{"", "true|0", "maybe|0|false|", "false|x|false|"} {
		t.Setenv("DOCKER_INSPECT_OUTPUT", out)
		if got, err := d.Status(context.Background(), "container"); err == nil {
			t.Fatalf("Status(%q) = %+v, want an error", out, got)
		}
	}
	logs, err := d.Logs(context.Background(), "container", true, 42)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(logs)
	_ = logs.Close()
	if err != nil || !strings.Contains(string(data), "startup complete") || !strings.Contains(string(data), "out of memory") {
		t.Fatalf("Logs = %q, err=%v", data, err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"logs", "-f", "--tail", "42", "container"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("fake docker args %q missing %q", args, want)
		}
	}
}

func TestPullImageReturnsDockerErrorTail(t *testing.T) {
	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf 'first progress line\\n'\nprintf 'permission denied while trying to connect to docker.sock\\n' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	var lines []string
	err := New().PullImage(context.Background(), "example.invalid/image:test", func(line string) {
		lines = append(lines, line)
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("PullImage error=%v, want captured docker stderr", err)
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "first progress line") || !strings.Contains(joined, "permission denied") {
		t.Fatalf("PullImage progress=%q", joined)
	}
}
