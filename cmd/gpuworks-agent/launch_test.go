package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agentconf"
)

func TestConfigureWritesTheFileWithTheTokenFromStdin(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	var out bytes.Buffer
	err := configure([]string{"--state", state, "--connect-mode", "server_dial", "--listen", "0.0.0.0:6006", "--driver", "udocker", "--udocker-bundle", "/b"}, strings.NewReader("node-token\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := agentconf.Load(state)
	if err != nil {
		t.Fatal(err)
	}
	want := agentconf.Config{Token: "node-token", ConnectMode: "server_dial", Listen: "0.0.0.0:6006", RuntimeDriver: "udocker", UdockerBundle: "/b"}
	if got != want {
		t.Fatalf("configured %+v, want %+v", got, want)
	}
	if !strings.Contains(out.String(), agentconf.Path(state)) {
		t.Fatalf("output %q does not name the file", out.String())
	}
}

func TestConfigureRefusesWhatTheAgentCannotStartWith(t *testing.T) {
	state := t.TempDir()
	for _, tc := range []struct {
		args  []string
		stdin string
		want  string
	}{
		{[]string{"--state", state}, "", "token is read from stdin"},
		{[]string{"--state", state}, "tok\n", "server URL is required"},
		{[]string{"--state", state, "--server", "gw:8800"}, "tok\n", "must start with http"},
		{[]string{"--state", state, "--connect-mode", "server_dial", "--listen", "x"}, "tok\n", "listen address"},
		{[]string{"--state", state, "extra"}, "tok\n", "unexpected argument"},
	} {
		err := configure(tc.args, strings.NewReader(tc.stdin), io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("configure %v: %v, want %q", tc.args, err, tc.want)
		}
	}
	if _, err := os.Stat(agentconf.Path(state)); !os.IsNotExist(err) {
		t.Fatalf("a refused configure left a file: %v", err)
	}
}

// A flag beats the environment, which beats the file; a missing file is no
// error (agents installed before it carry everything as flags).
func TestResolveLayersFlagsOverEnvironmentOverFile(t *testing.T) {
	state := t.TempDir()
	if err := agentconf.Write(state, agentconf.Config{Server: "http://file:8800", Token: "file-token", ConnectMode: "server_dial", Listen: ":6006", RuntimeDriver: "docker"}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GPUWORKS_TOKEN": "env-token", "GPUWORKS_RUNTIME_DRIVER": "udocker"}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	l := newLaunchFlags(fs)
	if err := fs.Parse([]string{"--state", state, "--driver", "nerdctl"}); err != nil {
		t.Fatal(err)
	}
	conf, dir, fromFile, err := l.resolve(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := agentconf.Config{Server: "http://file:8800", Token: "env-token", ConnectMode: "server_dial", Listen: ":6006", RuntimeDriver: "nerdctl"}
	if conf != want || dir != state || !fromFile {
		t.Fatalf("resolve = %+v %q %v, want %+v", conf, dir, fromFile, want)
	}
	if got := resumeArgs(conf, dir, fromFile); strings.Join(got, " ") != "--state "+state {
		t.Fatalf("resume args = %q", got)
	}

	fs = flag.NewFlagSet("run", flag.ContinueOnError)
	l = newLaunchFlags(fs)
	empty := t.TempDir()
	if err := fs.Parse([]string{"--state", empty, "--server", "http://flag:8800", "--token", "t", "--connect-mode", "server_dial", "--listen", ":7000"}); err != nil {
		t.Fatal(err)
	}
	conf, _, fromFile, err = l.resolve(func(string) string { return "" })
	if err != nil || fromFile || conf.Server != "http://flag:8800" {
		t.Fatalf("resolve without a file = %+v %v %v", conf, fromFile, err)
	}
	if got := strings.Join(resumeArgs(conf, empty, false), " "); got != "--server http://flag:8800 --token t --connect-mode server_dial --state "+empty+" --listen :7000" {
		t.Fatalf("legacy resume args = %q", got)
	}
}

func TestResolveReportsAnUnreadableFile(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(agentconf.Path(state), []byte("not a key value line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	l := newLaunchFlags(fs)
	_ = fs.Parse([]string{"--state", state})
	if _, _, _, err := l.resolve(func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), agentconf.FileName) {
		t.Fatalf("resolve = %v, want an error naming the file", err)
	}
}
