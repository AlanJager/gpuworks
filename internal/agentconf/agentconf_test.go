package agentconf

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderWritesEveryKeyInOrder(t *testing.T) {
	data, err := Config{
		Server: "https://gw.example:8800/", Token: "tok", RuntimeDriver: "udocker",
		MountRoot: "/data/mounts", UdockerBundle: "/opt/gpuworks/offline/udocker",
	}.Render()
	if err != nil {
		t.Fatal(err)
	}
	want := header + `SERVER=https://gw.example:8800
TOKEN=tok
CONNECT_MODE=agent_dial
LISTEN=
RUNTIME_DRIVER=udocker
MOUNT_ROOT=/data/mounts
UDOCKER_BUNDLE=/opt/gpuworks/offline/udocker
`
	if string(data) != want {
		t.Fatalf("render =\n%s\nwant\n%s", data, want)
	}
}

func TestServerDialRendersWithoutServerAndWithDefaultListen(t *testing.T) {
	data, err := Config{Token: "tok", ConnectMode: ServerDial}.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"\nSERVER=\n", "\nCONNECT_MODE=server_dial\n", "\nLISTEN=0.0.0.0:18800\n"} {
		if !strings.Contains(string(data), line) {
			t.Fatalf("render lacks %q:\n%s", line, data)
		}
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	in := Config{Server: "http://10.0.0.1:8800", Token: "a=b c", ConnectMode: ServerDial, Listen: "[::]:6006", RuntimeDriver: "docker"}
	data, err := in.Render()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if out != in.Normalized() {
		t.Fatalf("round trip = %+v, want %+v", out, in.Normalized())
	}
}

func TestParseTakesValuesLiterallyAndSkipsUnknownKeys(t *testing.T) {
	c, err := Parse([]byte("# comment\n\nTOKEN=$HOME 'x'\nFUTURE_KEY=1\r\nSERVER=http://s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "$HOME 'x'" || c.Server != "http://s" {
		t.Fatalf("parsed %+v", c)
	}
}

func TestParseRejectsMalformedLines(t *testing.T) {
	for _, data := range []string{"TOKEN", "=x", " TOKEN=x", "TOKEN=a\x00b"} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Fatalf("Parse(%q) accepted", data)
		}
	}
}

func TestValidateByDirection(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Config
		want error
	}{
		{"agent dial", Config{Server: "https://s", Token: "t"}, nil},
		{"agent dial needs server", Config{Token: "t"}, ErrServerRequired},
		{"server dial needs no server", Config{Token: "t", ConnectMode: ServerDial}, nil},
		{"server dial still checks a given server", Config{Server: "s:8800", Token: "t", ConnectMode: ServerDial}, ErrServerInvalid},
		{"token", Config{Server: "https://s"}, ErrTokenRequired},
		{"credentials in server", Config{Server: "https://u:p@s", Token: "t"}, ErrServerInvalid},
		{"query in server", Config{Server: "https://s/?a=1", Token: "t"}, ErrServerInvalid},
		{"mode", Config{Server: "https://s", Token: "t", ConnectMode: "both"}, ErrModeInvalid},
		{"listen port", Config{Token: "t", ConnectMode: ServerDial, Listen: "0.0.0.0:70000"}, ErrListenInvalid},
		{"listen form", Config{Token: "t", ConnectMode: ServerDial, Listen: "host"}, ErrListenInvalid},
		{"bare port listen", Config{Token: "t", ConnectMode: ServerDial, Listen: "6006"}, nil},
		{"agent dial ignores listen", Config{Server: "https://s", Token: "t", Listen: "junk"}, nil},
		{"newline", Config{Server: "https://s", Token: "t\nSERVER=x"}, ErrValueInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestWriteIsOwnerOnlyAndLoadReadsIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	c := Config{Token: "t", ConnectMode: ServerDial, Listen: ":6006"}
	if err := Write(dir, c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	got, err := Load(dir)
	if err != nil || got != c.Normalized() {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("Write left temporary files: %v", entries)
	}
	if _, err := Load(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load of a directory without the file = %v", err)
	}
	if err := Write(dir, Config{Token: "t"}); err == nil {
		t.Fatal("Write accepted an invalid configuration")
	}
}

// The shell functions read the file a unit's --state names, and an agent
// installed before the file existed from its arguments.
func TestShellFuncsReadTheFileOrLegacyArguments(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	state := t.TempDir()
	if err := Write(state, Config{Server: "https://new.example", Token: "new-token"}); err != nil {
		t.Fatal(err)
	}
	newLine := "/opt/gpuworks/gpuworks-agent run --state " + state
	oldLine := "{ path=/opt/gpuworks/gpuworks-agent ; argv[]=/opt/gpuworks/gpuworks-agent run --server https://old.example --token old-token --state /nonexistent ; ignore_errors=no }"
	script := ShellFuncs + `
printf '%s|%s|%s|%s\n' "$(gw_line_value "$NEW" SERVER)" "$(gw_line_value "$NEW" TOKEN)" "$(gw_line_value "$OLD" SERVER)" "$(gw_line_value "$OLD" TOKEN)"
`
	cmd := exec.Command(sh, "-c", script)
	cmd.Env = append(os.Environ(), "NEW="+newLine, "OLD="+oldLine)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "https://new.example|new-token|https://old.example|old-token"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
