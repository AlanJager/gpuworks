package dockerproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type call struct {
	line  string // "name arg arg"
	stdin string
}

// fakeRunner answers commands from a table keyed by the full command line;
// a command not in the table fails, like a missing binary would.
type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	// answers maps a command line to its stdout and error. A function value
	// lets a test change the answer over time.
	answers map[string]func() ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.calls = append(f.calls, call{line: line, stdin: string(stdin)})
	answer, ok := f.answers[line]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("exit status 127")
	}
	return answer()
}

func (f *fakeRunner) set(line string, out string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[line] = func() ([]byte, error) { return []byte(out), err }
}

func (f *fakeRunner) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.line
	}
	return out
}

func (f *fakeRunner) count(prefix string) int {
	n := 0
	for _, l := range f.lines() {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeRunner) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

const infoCmd = "docker info --format {{json .}}"

type env struct {
	m      *Manager
	r      *fakeRunner
	root   string
	now    time.Time
	nowMu  sync.Mutex
	sleeps int
}

// newEnv builds a Manager over a temp root that looks like a systemd host
// running as the given user.
func newEnv(t *testing.T, euid int, systemd bool, sudoOK bool) *env {
	t.Helper()
	e := &env{root: t.TempDir(), now: time.Unix(2_000_000, 0)}
	if systemd {
		if err := os.MkdirAll(filepath.Join(e.root, systemdRunDir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.r = &fakeRunner{answers: map[string]func() ([]byte, error){}}
	e.r.set(infoCmd, `{"HttpProxy":"","HttpsProxy":"","NoProxy":"","LiveRestoreEnabled":false}`, nil)
	if systemd {
		e.r.set("systemctl cat docker.service", "[Unit]", nil)
	}
	e.r.set("systemctl daemon-reload", "", nil)
	e.r.set("sudo -n systemctl daemon-reload", "", nil)
	if sudoOK {
		e.r.set("sudo -n true", "", nil)
	}
	e.m = &Manager{
		Root: e.root, Run: e.r, Euid: func() int { return euid },
		Now: func() time.Time { e.nowMu.Lock(); defer e.nowMu.Unlock(); return e.now },
		Sleep: func(d time.Duration) {
			e.nowMu.Lock()
			e.now = e.now.Add(d)
			e.sleeps++
			e.nowMu.Unlock()
		},
		RestartWait: 60 * time.Second,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return e
}

func (e *env) advance(d time.Duration) {
	e.nowMu.Lock()
	e.now = e.now.Add(d)
	e.nowMu.Unlock()
}

func (e *env) dropIn() (string, bool) {
	raw, err := os.ReadFile(filepath.Join(e.root, dropInDir, dropInName))
	return string(raw), err == nil
}

func (e *env) writeDropIn(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(e.root, dropInDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dropInName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

var proxyCfg = proto.NetworkConfig{HTTPProxy: "http://user:pw@proxy.corp:3128", HTTPSProxy: "http://user:pw@proxy.corp:3128", NoProxy: "10.0.0.0/8,.corp"}

func TestStatusObservesDaemonHostAndDropIn(t *testing.T) {
	e := newEnv(t, 0, true, false)
	e.r.set(infoCmd, `{"HttpProxy":"http://xxxxx:xxxxx@proxy.corp:3128","HttpsProxy":"http://xxxxx:xxxxx@proxy.corp:3128","NoProxy":"localhost,.corp","LiveRestoreEnabled":true,"Other":{"x":1}}`, nil)
	if err := os.MkdirAll(filepath.Join(e.root, "etc/docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, daemonJSON), []byte(`{"proxies":{"http-proxy":"http://p:1"},"log-driver":"json-file"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.writeDropIn(t, "[Service]\nEnvironment=\"HTTP_PROXY=http://user:secret@w:1\" \"HTTPS_PROXY=http://w:2\" \"NO_PROXY=a,b\"\n")

	st := e.m.Status(context.Background())
	want := proto.RuntimeProxyStatus{
		HTTPProxy: "http://xxxxx:xxxxx@proxy.corp:3128", HTTPSProxy: "http://xxxxx:xxxxx@proxy.corp:3128", NoProxy: "localhost,.corp",
		// The drop-in's password never leaves the node.
		WrittenHTTPProxy: "http://xxxxx:xxxxx@w:1", WrittenHTTPSProxy: "http://w:2", WrittenNoProxy: "a,b",
		LiveRestore: true, DaemonJSONProxy: true, Systemd: true, Privilege: "root",
	}
	if *st != want {
		t.Fatalf("status = %+v\nwant     %+v", *st, want)
	}
	if httpP, _, _, ok := e.m.Daemon(context.Background()); !ok || !strings.Contains(httpP, "xxxxx") {
		t.Fatalf("Daemon() = %q, %v", httpP, ok)
	}
}

func TestStatusDaemonJSONWithoutProxiesIsNotProxyConfig(t *testing.T) {
	for name, content := range map[string]string{
		"no proxies key": `{"log-driver":"json-file"}`,
		"empty proxies":  `{"proxies":{}}`,
		"malformed":      `{"proxies":`,
	} {
		e := newEnv(t, 0, true, false)
		if err := os.MkdirAll(filepath.Join(e.root, "etc/docker"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.root, daemonJSON), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if st := e.m.Status(context.Background()); st.DaemonJSONProxy {
			t.Errorf("%s: reported as a daemon.json proxy", name)
		}
	}
}

func TestStatusPrivilegeAndSystemdDetection(t *testing.T) {
	cases := []struct {
		name            string
		euid            int
		systemd, sudoOK bool
		privilege       string
	}{
		{"root", 0, true, false, "root"},
		{"passwordless sudo", 1000, true, true, "sudo"},
		{"no privilege", 1000, true, false, "none"},
	}
	for _, tc := range cases {
		e := newEnv(t, tc.euid, tc.systemd, tc.sudoOK)
		if st := e.m.Status(context.Background()); st.Privilege != tc.privilege || !st.Systemd {
			t.Errorf("%s: %+v", tc.name, *st)
		}
		if tc.euid == 0 && e.r.count("sudo") != 0 {
			t.Errorf("%s: root must not probe sudo", tc.name)
		}
	}
	// No /run/systemd/system: not systemd, and systemctl is never asked.
	e := newEnv(t, 0, false, false)
	if st := e.m.Status(context.Background()); st.Systemd || e.r.count("systemctl") != 0 {
		t.Errorf("non-systemd host reported systemd: %+v calls=%v", *st, e.r.lines())
	}
	// systemd present but docker is not a unit (e.g. docker run elsewhere).
	e = newEnv(t, 0, true, false)
	e.r.set("systemctl cat docker.service", "", errors.New("exit status 1"))
	if st := e.m.Status(context.Background()); st.Systemd {
		t.Errorf("a host without docker.service reported systemd: %+v", *st)
	}
}

func TestStatusReportsDockerInfoFailureAndKeepsTheRest(t *testing.T) {
	e := newEnv(t, 0, true, false)
	e.r.set(infoCmd, "", errors.New("exit status 1: Cannot connect to the Docker daemon"))
	st := e.m.Status(context.Background())
	if !strings.Contains(st.Error, "docker info failed") || st.Privilege != "root" || !st.Systemd {
		t.Fatalf("status = %+v", *st)
	}
	if _, _, _, ok := e.m.Daemon(context.Background()); ok {
		t.Fatal("Daemon() claimed an answer from a daemon that did not respond")
	}
	e.r.set(infoCmd, "not json", nil)
	e.advance(time.Minute)
	if st := e.m.Status(context.Background()); !strings.Contains(st.Error, "unparsable") {
		t.Fatalf("garbage docker info accepted: %+v", *st)
	}
}

func TestStatusIsCachedForThirtySeconds(t *testing.T) {
	e := newEnv(t, 0, true, false)
	e.m.Status(context.Background())
	e.m.Status(context.Background())
	e.advance(29 * time.Second)
	e.m.Status(context.Background())
	if n := e.r.count("docker info"); n != 1 {
		t.Fatalf("docker info ran %d times within the cache window", n)
	}
	e.advance(2 * time.Second)
	e.m.Status(context.Background())
	if n := e.r.count("docker info"); n != 2 {
		t.Fatalf("docker info ran %d times after the cache expired", n)
	}
}

func TestApplyWritesDropInAsRootAndReloadsWithoutRestart(t *testing.T) {
	e := newEnv(t, 0, true, false)
	if err := e.m.Apply(context.Background(), proxyCfg, "https://gw.example.com:8800"); err != nil {
		t.Fatal(err)
	}
	content, ok := e.dropIn()
	if !ok {
		t.Fatal("drop-in not written")
	}
	for _, want := range []string{
		"[Service]\n",
		`"HTTP_PROXY=http://user:pw@proxy.corp:3128"`,
		`"HTTPS_PROXY=http://user:pw@proxy.corp:3128"`,
		`"NO_PROXY=10.0.0.0/8,.corp,localhost,127.0.0.1,::1,gw.example.com"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("drop-in lacks %s:\n%s", want, content)
		}
	}
	if info, _ := os.Stat(filepath.Join(e.root, dropInDir, dropInName)); info.Mode().Perm() != 0o644 {
		t.Errorf("drop-in mode %v", info.Mode().Perm())
	}
	if e.r.count("systemctl daemon-reload") != 1 || e.r.count("systemctl restart") != 0 || e.r.count("sudo") != 0 {
		t.Fatalf("commands = %v", e.r.lines())
	}
	st := e.m.Status(context.Background())
	if st.WrittenHTTPProxy != "http://xxxxx:xxxxx@proxy.corp:3128" || !strings.HasSuffix(st.WrittenNoProxy, "gw.example.com") || st.Error != "" {
		t.Fatalf("status after apply = %+v", *st)
	}
}

func TestApplyIsANoOpWhenNothingChanges(t *testing.T) {
	e := newEnv(t, 0, true, false)
	// No proxy configured and no drop-in: not a single command, not even a probe.
	if err := e.m.Apply(context.Background(), proto.NetworkConfig{}, "https://gw.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := e.r.lines(); len(got) != 0 {
		t.Fatalf("unconfigured node ran commands: %v", got)
	}
	if _, ok := e.dropIn(); ok {
		t.Fatal("drop-in created without a proxy")
	}
	// Configured and already written: second apply does nothing.
	if err := e.m.Apply(context.Background(), proxyCfg, "https://gw.example.com"); err != nil {
		t.Fatal(err)
	}
	e.r.reset()
	if err := e.m.Apply(context.Background(), proxyCfg, "https://gw.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := e.r.lines(); len(got) != 0 {
		t.Fatalf("unchanged config ran commands: %v", got)
	}
}

func TestApplyRewritesChangedProxyAndRemovesDropInWhenProxyCleared(t *testing.T) {
	e := newEnv(t, 0, true, false)
	ctx := context.Background()
	if err := e.m.Apply(ctx, proxyCfg, "https://gw.example.com"); err != nil {
		t.Fatal(err)
	}
	changed := proxyCfg
	changed.HTTPProxy, changed.HTTPSProxy = "http://other.corp:8080", "http://other.corp:8080"
	e.r.reset()
	if err := e.m.Apply(ctx, changed, "https://gw.example.com"); err != nil {
		t.Fatal(err)
	}
	if content, _ := e.dropIn(); !strings.Contains(content, "other.corp:8080") || strings.Contains(content, "proxy.corp") {
		t.Fatalf("drop-in not rewritten:\n%s", content)
	}
	if e.r.count("systemctl daemon-reload") != 1 {
		t.Fatalf("commands = %v", e.r.lines())
	}
	// The proxy is cleared on the server: the drop-in goes, systemd reloads.
	e.r.reset()
	if err := e.m.Apply(ctx, proto.NetworkConfig{}, "https://gw.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.dropIn(); ok {
		t.Fatal("drop-in survived clearing the proxy")
	}
	if e.r.count("systemctl daemon-reload") != 1 || e.r.count("systemctl restart") != 0 {
		t.Fatalf("commands = %v", e.r.lines())
	}
	// Removing an already removed drop-in is quiet.
	e.r.reset()
	if err := e.m.Apply(ctx, proto.NetworkConfig{}, "https://gw.example.com"); err != nil || len(e.r.lines()) != 0 {
		t.Fatalf("second removal: err=%v commands=%v", err, e.r.lines())
	}
}

func TestApplyFillsTheMissingSchemeFromTheOther(t *testing.T) {
	e := newEnv(t, 0, true, false)
	if err := e.m.Apply(context.Background(), proto.NetworkConfig{HTTPSProxy: "proxy.corp:3128"}, ""); err != nil {
		t.Fatal(err)
	}
	content, _ := e.dropIn()
	if !strings.Contains(content, `"HTTP_PROXY=proxy.corp:3128" "HTTPS_PROXY=proxy.corp:3128" "NO_PROXY=localhost,127.0.0.1,::1"`) {
		t.Fatalf("drop-in:\n%s", content)
	}
}

func TestApplyThroughSudoNeverWritesLocally(t *testing.T) {
	e := newEnv(t, 1000, true, true)
	ctx := context.Background()
	dropIn := filepath.Join(e.root, dropInDir, dropInName)
	e.r.set("sudo -n mkdir -p "+filepath.Dir(dropIn), "", nil)
	e.r.set("sudo -n tee "+dropIn, "", nil)
	if err := e.m.Apply(ctx, proxyCfg, "https://gw.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.dropIn(); ok {
		t.Fatal("agent wrote the root-owned directory itself")
	}
	var tee call
	for _, c := range e.r.calls {
		if c.line == "sudo -n tee "+dropIn {
			tee = c
		}
	}
	if !strings.Contains(tee.stdin, `"HTTPS_PROXY=http://user:pw@proxy.corp:3128"`) {
		t.Fatalf("tee did not receive the drop-in on stdin: %v", e.r.lines())
	}
	for _, c := range e.r.calls {
		if strings.Contains(c.line, "pw@") {
			t.Fatalf("proxy credentials on a command line: %q", c.line)
		}
	}
	lines := e.r.lines()
	if e.r.count("sudo -n mkdir -p "+filepath.Dir(dropIn)) != 1 || e.r.count("sudo -n systemctl daemon-reload") != 1 || e.r.count("sudo -n systemctl restart") != 0 {
		t.Fatalf("commands = %v", lines)
	}

	// Removal through sudo.
	e.writeDropIn(t, "[Service]\nEnvironment=\"HTTP_PROXY=http://old:1\"\n")
	e.r.set("sudo -n rm -f "+dropIn, "", nil)
	e.r.reset()
	if err := e.m.Apply(ctx, proto.NetworkConfig{}, ""); err != nil {
		t.Fatal(err)
	}
	if e.r.count("sudo -n rm -f "+dropIn) != 1 || e.r.count("sudo -n systemctl daemon-reload") != 1 {
		t.Fatalf("commands = %v", e.r.lines())
	}
}

func TestApplyWithoutPrivilegeOrSystemdChangesNothingAndSaysWhy(t *testing.T) {
	// No root, no sudo.
	e := newEnv(t, 1000, true, false)
	err := e.m.Apply(context.Background(), proxyCfg, "https://gw.example.com")
	if err == nil || !strings.Contains(err.Error(), "neither root nor passwordless sudo") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := e.dropIn(); ok || e.r.count("sudo -n tee") != 0 || e.r.count("systemctl daemon-reload") != 0 {
		t.Fatalf("an unprivileged agent touched the system: %v", e.r.lines())
	}
	if st := e.m.Status(context.Background()); !strings.Contains(st.Error, "neither root nor passwordless sudo") || st.Privilege != "none" {
		t.Fatalf("status does not explain the unapplied proxy: %+v", *st)
	}
	// Not systemd.
	e = newEnv(t, 0, false, false)
	if err := e.m.Apply(context.Background(), proxyCfg, ""); err == nil || !strings.Contains(err.Error(), "systemd") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := e.dropIn(); ok {
		t.Fatal("drop-in written on a host without systemd")
	}
	// The explanation goes away once the config no longer asks for a proxy.
	if err := e.m.Apply(context.Background(), proto.NetworkConfig{}, ""); err != nil {
		t.Fatal(err)
	}
	if st := e.m.Status(context.Background()); strings.Contains(st.Error, "systemd") {
		t.Fatalf("stale apply error: %+v", *st)
	}
}

func TestApplyWriteAndReloadFailuresAreReportedNotFatal(t *testing.T) {
	// The drop-in directory cannot be created: etc is a regular file.
	e := newEnv(t, 0, true, false)
	if err := os.WriteFile(filepath.Join(e.root, "etc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Apply(context.Background(), proxyCfg, ""); err == nil || !strings.Contains(err.Error(), "write docker proxy drop-in") {
		t.Fatalf("err = %v", err)
	}
	if st := e.m.Status(context.Background()); !strings.Contains(st.Error, "write docker proxy drop-in") {
		t.Fatalf("status = %+v", *st)
	}
	if e.r.count("systemctl daemon-reload") != 0 {
		t.Fatal("daemon-reload ran after a failed write")
	}

	// tee fails under sudo.
	e = newEnv(t, 1000, true, true)
	dropIn := filepath.Join(e.root, dropInDir, dropInName)
	e.r.set("sudo -n mkdir -p "+filepath.Dir(dropIn), "", nil)
	e.r.set("sudo -n tee "+dropIn, "", errors.New("tee: Permission denied"))
	if err := e.m.Apply(context.Background(), proxyCfg, ""); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err = %v", err)
	}
	if e.r.count("sudo -n systemctl daemon-reload") != 0 {
		t.Fatal("daemon-reload ran after a failed tee")
	}

	// daemon-reload fails after the file was written; the next apply retries
	// instead of believing the drop-in is live.
	e = newEnv(t, 0, true, false)
	e.r.set("systemctl daemon-reload", "", errors.New("exit status 1"))
	if err := e.m.Apply(context.Background(), proxyCfg, ""); err == nil || !strings.Contains(err.Error(), "daemon-reload") {
		t.Fatalf("err = %v", err)
	}
	if st := e.m.Status(context.Background()); !strings.Contains(st.Error, "daemon-reload") {
		t.Fatalf("status = %+v", *st)
	}
}

func TestRestartRefusals(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 1000, true, false)
	if _, err := e.m.Restart(ctx); err == nil || !strings.Contains(err.Error(), "neither root nor passwordless sudo") {
		t.Fatalf("unprivileged restart: %v", err)
	}
	e = newEnv(t, 0, false, false)
	if _, err := e.m.Restart(ctx); err == nil || !strings.Contains(err.Error(), "systemd") {
		t.Fatalf("non-systemd restart: %v", err)
	}
	for _, e := range []*env{e, newEnv(t, 1000, true, false)} {
		if e.r.count("systemctl restart") != 0 || e.r.count("sudo -n systemctl restart") != 0 {
			t.Fatalf("a refused restart still ran: %v", e.r.lines())
		}
	}
}

func TestRestartRestartsWaitsForTheDaemonAndReturnsFreshStatus(t *testing.T) {
	e := newEnv(t, 1000, true, true)
	e.r.set("sudo -n systemctl restart docker", "", nil)
	var mu sync.Mutex
	infoCalls := 0
	e.r.mu.Lock()
	e.r.answers[infoCmd] = func() ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		infoCalls++
		switch {
		case infoCalls == 1: // the status read that precedes the restart
			return []byte(`{"HttpProxy":"","HttpsProxy":"","NoProxy":""}`), nil
		case infoCalls <= 3: // the daemon is still coming up
			return nil, errors.New("Cannot connect to the Docker daemon")
		default:
			return []byte(`{"HttpProxy":"http://xxxxx:xxxxx@proxy.corp:3128","HttpsProxy":"http://xxxxx:xxxxx@proxy.corp:3128","NoProxy":"localhost","LiveRestoreEnabled":true}`), nil
		}
	}
	e.r.mu.Unlock()
	st, err := e.m.Restart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if e.r.count("sudo -n systemctl restart docker") != 1 {
		t.Fatalf("commands = %v", e.r.lines())
	}
	if st.HTTPProxy != "http://xxxxx:xxxxx@proxy.corp:3128" || !st.LiveRestore || st.Privilege != "sudo" {
		t.Fatalf("status = %+v", *st)
	}
	if e.sleeps < 2 {
		t.Fatalf("restart did not wait for the daemon (sleeps=%d)", e.sleeps)
	}
	// The observation after the restart replaced the cached one.
	if httpP, _, _, ok := e.m.Daemon(context.Background()); !ok || !strings.Contains(httpP, "proxy.corp") {
		t.Fatalf("stale observation after restart: %q", httpP)
	}
}

func TestRestartAsRootUsesSystemctlDirectly(t *testing.T) {
	e := newEnv(t, 0, true, false)
	e.r.set("systemctl restart docker", "", nil)
	if _, err := e.m.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.r.count("systemctl restart docker") != 1 || e.r.count("sudo") != 0 {
		t.Fatalf("commands = %v", e.r.lines())
	}
}

func TestRestartFailureAndDaemonThatNeverReturns(t *testing.T) {
	e := newEnv(t, 0, true, false)
	e.r.set("systemctl restart docker", "", errors.New("exit status 1: Job for docker.service failed"))
	if _, err := e.m.Restart(context.Background()); err == nil || !strings.Contains(err.Error(), "Job for docker.service failed") {
		t.Fatalf("err = %v", err)
	}

	e = newEnv(t, 0, true, false)
	e.r.set("systemctl restart docker", "", nil)
	var mu sync.Mutex
	first := true
	e.r.mu.Lock()
	e.r.answers[infoCmd] = func() ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if first { // the pre-restart status read
			first = false
			return []byte(`{}`), nil
		}
		return nil, errors.New("Cannot connect to the Docker daemon")
	}
	e.r.mu.Unlock()
	_, err := e.m.Restart(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not answer within 1m0s") {
		t.Fatalf("err = %v", err)
	}
	if e.sleeps < 30 {
		t.Fatalf("gave up before the patience ran out (sleeps=%d)", e.sleeps)
	}
}

func TestRestartHonoursCancellation(t *testing.T) {
	e := newEnv(t, 0, true, false)
	e.r.set("systemctl restart docker", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	e.r.mu.Lock()
	e.r.answers[infoCmd] = func() ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{}`), nil
		}
		cancel()
		return nil, errors.New("down")
	}
	e.r.mu.Unlock()
	if _, err := e.m.Restart(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestDropInRoundTripEscaping(t *testing.T) {
	p := proxyEnv{HTTP: `http://u:p"w\d%20@h:1`, HTTPS: "http://h:2", No: "a b,c"}
	content, err := renderDropIn(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := parseDropIn(content); got != p {
		t.Fatalf("round trip = %+v, want %+v\n%s", got, p, content)
	}
	if _, err := renderDropIn(proxyEnv{HTTP: "http://h\nExecStart=/bin/evil"}); err == nil {
		t.Fatal("a newline in a proxy value would inject a unit directive")
	}
	if parseDropIn("garbage\nEnvironment=NOEQUALS\n") != (proxyEnv{}) {
		t.Fatal("unrelated content parsed as a proxy")
	}
}

func TestNoProxyForMergesWithoutRepeats(t *testing.T) {
	cases := []struct{ user, server, want string }{
		{"", "", "localhost,127.0.0.1,::1"},
		{"10.0.0.0/8, .corp ,LOCALHOST", "https://gw.example.com:8800/", "10.0.0.0/8,.corp,LOCALHOST,127.0.0.1,::1,gw.example.com"},
		{"gw.example.com", "https://gw.example.com", "gw.example.com,localhost,127.0.0.1,::1"},
		{"", "http://[fd00::1]:8800", "localhost,127.0.0.1,::1,fd00::1"},
		{"", "10.1.2.3:8800", "localhost,127.0.0.1,::1,10.1.2.3"},
	}
	for _, tc := range cases {
		if got := NoProxyFor(tc.user, tc.server); got != tc.want {
			t.Errorf("NoProxyFor(%q, %q) = %q, want %q", tc.user, tc.server, got, tc.want)
		}
	}
}

func TestRedactDropsCredentials(t *testing.T) {
	if got := redact("http://user:secret@proxy.corp:3128"); strings.Contains(got, "secret") || strings.Contains(got, "user") || !strings.Contains(got, "proxy.corp:3128") {
		t.Fatalf("redact = %q", got)
	}
}

func TestExecRunnerReturnsStdoutAndStderrTailInErrors(t *testing.T) {
	out, err := execRunner{}.Run(context.Background(), []byte("hello"), "cat")
	if err != nil || string(out) != "hello" {
		t.Fatalf("cat: %q %v", out, err)
	}
	_, err = execRunner{}.Run(context.Background(), nil, "sh", "-c", "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("stderr missing from error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (execRunner{}).Run(ctx, nil, "sleep", "5"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

// What dockerd starts with is systemd's loaded environment of the unit (every
// drop-in, the platform's and an operator's), masked like docker info masks it;
// a lower-case variable counts when the upper-case one is unset, as it does
// for dockerd. Without an answer from systemd it is unknown.
func TestStatusReportsTheProxySystemdStartsDockerdWith(t *testing.T) {
	e := newEnv(t, 0, true, false)
	const show = "systemctl show docker.service --property=Environment --value"
	e.r.set(show, `HTTP_PROXY=http://user:pw@proxy.corp:3128 https_proxy=http://own.example:7890 "NO_PROXY=a, b"`+"\n", nil)
	st := e.m.Status(context.Background())
	if !st.ConfiguredKnown || st.ConfiguredHTTPProxy != "http://xxxxx:xxxxx@proxy.corp:3128" || st.ConfiguredHTTPSProxy != "http://own.example:7890" {
		t.Fatalf("configured = %q %q known=%v", st.ConfiguredHTTPProxy, st.ConfiguredHTTPSProxy, st.ConfiguredKnown)
	}

	e = newEnv(t, 0, true, false)
	e.r.set(show, "\n", nil)
	if st := e.m.Status(context.Background()); !st.ConfiguredKnown || st.ConfiguredHTTPProxy != "" || st.ConfiguredHTTPSProxy != "" {
		t.Fatalf("a unit without proxy variables = %+v", st)
	}

	e = newEnv(t, 0, true, false) // systemctl show fails
	if st := e.m.Status(context.Background()); st.ConfiguredKnown {
		t.Fatalf("an unanswered systemctl reads as known: %+v", st)
	}
	e = newEnv(t, 0, false, false) // not systemd: never asked
	if st := e.m.Status(context.Background()); st.ConfiguredKnown || e.r.count("systemctl show") != 0 {
		t.Fatalf("a host without systemd = %+v calls=%v", st, e.r.lines())
	}
}
