package app

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestInstallHandlerRendersTokenArgumentAndOverrides(t *testing.T) {
	a := &App{}
	req := httptest.NewRequest(http.MethodGet, "http://ignored.example/install.sh", nil)
	res := httptest.NewRecorder()
	a.installHandler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	body := res.Body.String()
	for _, want := range []string{
		`SERVER_URL=""`,
		`--server is required`,
		`--server) SERVER_URL="$2"`,
		`TOKEN=""`,
		`--token is required`,
		`--token) TOKEN="$2"`,
		`--connect-mode) CONNECT_MODE="$2"`,
		`--listen) LISTEN_ADDR="$2"`,
		`--driver) RUNTIME_DRIVER="$2"`,
		`--install-dir) INSTALL_DIR="$2"`,
		`printf '%s\n' "$TOKEN" | "$BIN" configure "$@"`,
		`ExecStart=$BIN run --state $STATE_DIR`,
		`nohup "$BIN" run --state "$STATE_DIR"`,
		`gpuworks-agent-${GOOS}-${GOARCH}`,
		`sudo -n true`,
		`$SUDO mkdir -p "$INSTALL_DIR" "$STATE_DIR"`,
		`cat <<GPUWORKS_AGENT_UNIT | $SUDO tee /etc/systemd/system/gpuworks-agent.service >/dev/null`,
		`$SUDO systemctl daemon-reload`,
		`$SUDO systemctl enable gpuworks-agent`,
		`$SUDO systemctl restart gpuworks-agent`,
		`TMP_BIN="${BIN}.tmp.$$"`,
		`mv -f "$TMP_BIN" "$BIN"`,
		`$SUDO "$BIN" grant-device-access --state "$STATE_DIR" --user udocker --create-user`,
		`PID_FILE="$STATE_DIR/gpuworks-agent.pid"`,
		`[ "$(tr '\000' '\n' < "/proc/$OLD_PID/cmdline" | sed -n '1p')" = "$BIN" ]`,
		`awk -v state="$STATE_DIR" 'previous == "--state" && $0 == state`,
		`printf '%s\n' "$AGENT_PID" > "$PID_FILE"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("install script missing %q", want)
		}
	}
	if strings.Contains(body, "pkill -f") {
		t.Fatal("install script must not use broad process matching")
	}
	if strings.Contains(body, "--token ${TOKEN}") || strings.Contains(body, `--token "$TOKEN"`) {
		t.Fatal("install script puts the token on a command line")
	}
	assertShellSyntax(t, body)
}

// The script never carries a server address of its own: it is the same for
// every request, however the request reached the server, and it refuses to
// run without the --server the install command gives.
func TestInstallScriptTakesTheServerFromTheCommand(t *testing.T) {
	a := &App{cfg: Config{TrustedProxies: "192.0.2.1/32"}}
	fetch := func(target string, header map[string]string) string {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RemoteAddr = "192.0.2.1:9000"
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res := httptest.NewRecorder()
		a.installHandler().ServeHTTP(res, req)
		return res.Body.String()
	}
	direct := fetch("http://10.0.0.8:8800/install.sh", nil)
	proxied := fetch("http://127.0.0.1:8800/install.sh", map[string]string{"X-Forwarded-Host": "gpuworks.example", "X-Forwarded-Proto": "https"})
	if direct != proxied || strings.Contains(direct, "gpuworks.example") || strings.Contains(direct, "10.0.0.8") {
		t.Fatal("the script depends on how it was fetched")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for args, want := range map[string]string{
		"--token t":         "--server is required",
		"--server http://s": "--token is required",
	} {
		cmd := exec.Command("sh", append([]string{"-c", direct, "install.sh"}, strings.Fields(args)...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "GPUWORKS_INSTALL_DIR=" + t.TempDir()}
		out, err := cmd.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), want) {
			t.Fatalf("%s: exit %v output %q", args, err, out)
		}
	}
}

func TestInstallHandlerDoesNotInjectBootstrapSecret(t *testing.T) {
	a := &App{}
	req := httptest.NewRequest(http.MethodGet, "https://control.example/install.sh", nil)
	res := httptest.NewRecorder()
	a.installHandler().ServeHTTP(res, req)
	if strings.Contains(res.Body.String(), "GPUWORKS_BOOTSTRAP_TOKEN") {
		t.Fatal("install script referenced deprecated bootstrap token")
	}
}

func assertShellSyntax(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell syntax: %v: %s", err, out)
	}
}
