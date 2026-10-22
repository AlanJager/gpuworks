package agentconf

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShellStateDirectoryPreservesSpaces(t *testing.T) {
	state := filepath.Join(t.TempDir(), "gpu works;data")
	if err := Write(state, Config{Server: "http://gw", Token: "node-token"}); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		state + "/gpuworks-agent run --state " + state,
		state + "/gpuworks-agent run --state=" + state,
		state + "/gpuworks-agent run --state " + state + " --connect-mode agent_dial --listen :19090",
		"{ path=" + state + "/gpuworks-agent ; argv[]=" + state + "/gpuworks-agent run --state " + state + " ; ignore_errors=no ; pid=123 ; }",
		"{ argv[]=" + state + "/gpuworks-agent run --state " + state + " ; }",
		"{ path=" + state + "/gpuworks-agent ; argv[]=" + state + "/gpuworks-agent run --server http://old --token old-token --state " + state + " --listen :19090 ; ignore_errors=no ; }",
	} {
		cmd := exec.Command("sh", "-c", ShellFuncs+`
printf '%s\n' "$LINE" | gw_arg state
gw_line_value "$LINE" SERVER
gw_line_value "$LINE" TOKEN
`)
		cmd.Env = append(os.Environ(), "LINE="+line)
		out, err := cmd.CombinedOutput()
		want := state + "\nhttp://gw\nnode-token\n"
		if err != nil || string(out) != want {
			t.Fatalf("line=%q: got=%q err=%v, want=%q", line, out, err, want)
		}
	}
}

func TestShellStateDirectoryDoesNotConsumeLegacyOptions(t *testing.T) {
	state := filepath.Join(t.TempDir(), "legacy state")
	line := "/opt/agent run --state " + state + " --server http://legacy --token legacy-token --connect-mode agent_dial"
	cmd := exec.Command("sh", "-c", ShellFuncs+`
printf '%s\n' "$LINE" | gw_arg state
gw_line_value "$LINE" SERVER
gw_line_value "$LINE" TOKEN
`)
	cmd.Env = append(os.Environ(), "LINE="+line)
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != state+"\nhttp://legacy\nlegacy-token" {
		t.Fatalf("legacy values=%q err=%v", out, err)
	}
}
