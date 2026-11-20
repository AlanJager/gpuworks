package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RootPID asks the CLI for the container's init process as the host numbers
// it. A container that is not running answers 0, which is no process; a
// missing container and an unparseable answer are errors, never a pid.
func TestRootPIDParsesFakeDockerCLI(t *testing.T) {
	binDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	script := strings.Join([]string{
		"#!/bin/sh",
		"printf '%s\\n' \"$@\" > \"$DOCKER_ARGS_FILE\"",
		"if [ \"$4\" = missing ]; then printf '%s\\n' 'Error response from daemon: No such container: missing' >&2; exit 1; fi",
		"printf '%s' \"$DOCKER_INSPECT_OUTPUT\"",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("DOCKER_ARGS_FILE", argsFile)
	d := New()
	for _, tc := range []struct {
		name, out string
		want      int
		wantErr   bool
	}{
		{name: "running", out: "41873\n", want: 41873},
		{name: "a warning before the answer", out: "WARNING: daemon config reload pending\n41873\n", want: 41873},
		{name: "exited or created: no process", out: "0\n", wantErr: true},
		{name: "empty answer", out: "", wantErr: true},
		{name: "not a number", out: "<no value>\n", wantErr: true},
		{name: "negative", out: "-1\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_INSPECT_OUTPUT", tc.out)
			got, err := d.RootPID(context.Background(), "container")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("RootPID = %d, err=%v; want %d, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "inspect\n--format\n{{.State.Pid}}\ncontainer\n"; string(args) != want {
		t.Fatalf("docker was asked %q, want %q", args, want)
	}
	if got, err := d.RootPID(context.Background(), "missing"); err == nil || got != 0 {
		t.Fatalf("a missing container: RootPID = %d, err=%v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if got, err := d.RootPID(context.Background(), "container"); err == nil || got != 0 {
		t.Fatalf("no docker CLI: RootPID = %d, err=%v", got, err)
	}
}
