package slurmboot

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testServer = "https://gw.example:8800"

// shellUnderTest is one POSIX shell the scripts must work with.
type shellUnderTest struct{ name, path string }

// shells lists the installed shells (busybox ash is left out: it runs its own
// applets instead of the fakes on PATH); the scripts run under each of them. A
// shell that is not installed is skipped, but at least one must be there.
func shells(t *testing.T) []shellUnderTest {
	t.Helper()
	var out []shellUnderTest
	for _, c := range []struct{ name, path string }{
		{"dash", "/usr/bin/dash"},
		{"bash", "/usr/bin/bash"},
	} {
		if _, err := os.Stat(c.path); err == nil {
			out = append(out, shellUnderTest{c.name, c.path})
		} else {
			t.Logf("shell %s not installed, skipped", c.name)
		}
	}
	if len(out) == 0 {
		t.Fatal("no POSIX shell to test with")
	}
	return out
}

// eachShell runs fn as a subtest per shell. The tests below call it first so
// that every behaviour is proven under dash and under bash in POSIX mode.
func eachShell(t *testing.T, fn func(t *testing.T, w *world)) {
	t.Helper()
	for _, sh := range shells(t) {
		t.Run(sh.name, func(t *testing.T) { fn(t, newWorld(t, sh)) })
	}
}

// world is one fake machine: a PATH holding only fakes and symlinks to the
// basic tools, a state directory, and a directory per path the scripts use.
type world struct {
	t     *testing.T
	sh    shellUnderTest
	root  string
	bin   string
	state string
	env   map[string]string
	p     Params
	// noToken keeps installCheck from writing the token file.
	noToken bool
}

func newWorld(t *testing.T, sh shellUnderTest) *world {
	t.Helper()
	root, err := os.MkdirTemp("", "slurmboot")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	w := &world{t: t, sh: sh, root: root, bin: filepath.Join(root, "bin"), state: filepath.Join(root, "state"), env: map[string]string{}}
	for _, d := range []string{w.bin, w.state, w.path("dev"), w.path("proc"), w.path("run"), w.path("etc/slurm"), w.path("log")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(sh.path, filepath.Join(w.bin, "sh")); err != nil {
		t.Fatal(err)
	}
	for _, tool := range realTools {
		src, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		if err := os.Symlink(src, filepath.Join(w.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range fakeTools {
		w.writeExec(filepath.Join(w.bin, name), body)
	}
	w.p = Params{ServerURL: testServer, Token: "tok-abc.123_X", RunAs: "root",
		InstallDir: w.path("var/gpuworks"), ScriptDir: w.path("opt/gwks")}
	w.env["FAKE_SCRIPT_DIR"] = w.p.ScriptDir
	w.env["FAKE_SLURM_CONF"] = w.path("etc/slurm/slurm.conf")
	w.env["FAKE_SERVER"] = testServer
	w.env["FAKE_TOKEN"] = w.p.Token
	w.env["FAKE_UNIT_STATE"] = w.p.InstallDir
	return w
}

func (w *world) path(rel string) string { return filepath.Join(w.root, rel) }

func (w *world) writeExec(path, body string) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) write(path, body string) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) read(path string) string {
	w.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

func (w *world) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// flag creates (or with on=false removes) an empty state file.
func (w *world) flag(name string, on bool) {
	w.t.Helper()
	p := filepath.Join(w.state, name)
	if on {
		w.write(p, "")
	} else {
		_ = os.Remove(p)
	}
}

func (w *world) stateFile(name string) string {
	b, _ := os.ReadFile(filepath.Join(w.state, name))
	return string(b)
}

// remove takes one tool off the PATH.
func (w *world) remove(tool string) {
	w.t.Helper()
	if err := os.Remove(filepath.Join(w.bin, tool)); err != nil {
		w.t.Fatal(err)
	}
}

type result struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
}

func (w *world) environ() []string {
	env := []string{
		"PATH=" + w.bin, "HOME=" + w.root, "TMPDIR=" + w.path("run"), "LC_ALL=C",
		"GWKS_NO_PATH_EXTEND=1", "GWKS_FAKE_STATE=" + w.state,
		"GWKS_DEV_DIR=" + w.path("dev"), "GWKS_PROC_DIR=" + w.path("proc"), "GWKS_LOCK_DIR=" + w.path("run"),
		"GWKS_LOG_DIR=" + w.path("log"), "GWKS_UNIT_PATH=" + w.path("etc/gpuworks-agent.service"), "GWKS_SPREAD_MAX=0",
	}
	for k, v := range w.env {
		env = append(env, k+"="+v)
	}
	return env
}

func (w *world) exec(stdin string, args ...string) result {
	w.t.Helper()
	cmd := exec.Command(filepath.Join(w.bin, "sh"), args...)
	cmd.Env = w.environ()
	cmd.Dir = w.root
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String(), elapsed: time.Since(start)}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	case err != nil:
		w.t.Fatalf("running %v: %v", args, err)
	}
	return r
}

// installCheck writes check.sh the way setup.sh would, for tests that run it
// without going through setup.
func (w *world) installCheck() string {
	w.t.Helper()
	path := filepath.Join(w.p.ScriptDir, "check.sh")
	w.writeExec(path, CheckScript(w.p))
	tokenPath := filepath.Join(w.p.ScriptDir, TokenFileName)
	if !w.exists(tokenPath) && !w.noToken {
		if err := os.WriteFile(tokenPath, []byte(w.p.Token+"\n"), 0o600); err != nil {
			w.t.Fatal(err)
		}
	}
	return path
}

func (w *world) runCheck(args ...string) result {
	w.t.Helper()
	return w.exec("", append([]string{w.installCheck()}, args...)...)
}

// runSetup feeds setup.sh through stdin, the way `curl | sudo sh` does.
func (w *world) runSetup(args ...string) result {
	w.t.Helper()
	return w.exec(SetupScript(w.p), append([]string{"-s", "--"}, args...)...)
}

// program writes an executable that records its arguments and exits with code.
func (w *world) program(name string, code int) string {
	w.t.Helper()
	p := filepath.Join(w.root, "progs", name)
	w.writeExec(p, "#!/bin/sh\necho \"$*\" >> \""+p+".args\"\nexit "+strconv.Itoa(code)+"\n")
	return p
}

// waitFor polls until cond holds.
func (w *world) waitFor(what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.t.Fatalf("timed out waiting for %s", what)
}

// installs splits the installer log into one entry per run.
func (w *world) installs() []string {
	var out []string
	for _, c := range strings.Split(w.stateFile("installer"), "--- install\n") {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}
