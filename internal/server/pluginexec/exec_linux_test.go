//go:build linux

package pluginexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The test binary is also the trampoline (argv[1] plugin-exec) and the
// "plugin" it executes (argv[1] probe), so these tests run the real
// sandbox in real processes.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case Command:
			if os.Getenv("PLUGINEXEC_TEST_NO_LANDLOCK") == "1" {
				landlockABI = func() int { return 0 }
			}
			os.Exit(Main(os.Args[2:]))
		case "probe":
			os.Exit(probe(os.Args[2:]))
		}
	}
	os.Exit(m.Run())
}

// probeReport is what the probe sees from inside the sandbox.
type probeReport struct {
	PDeathSig  int      `json:"pdeathsig"`
	NoNewPrivs int      `json:"no_new_privs"`
	NoFile     uint64   `json:"nofile"`
	Core       uint64   `json:"core"`
	Nice       int      `json:"nice"`
	OwnGroup   bool     `json:"own_group"`
	Cwd        string   `json:"cwd"`
	Env        []string `json:"env"`
	StdinNull  bool     `json:"stdin_null"`
	StdoutNull bool     `json:"stdout_null"`
	FD3Socket  bool     `json:"fd3_socket"`
	FD4Socket  bool     `json:"fd4_socket"`
	Outside    string   `json:"outside"`
	Inside     string   `json:"inside"`
	Signal     string   `json:"signal"`
}

// probe reports its environment as one JSON line on stderr (stdout is
// /dev/null): argv is <path outside the sandbox> <writable dir>.
func probe(argv []string) int {
	var r probeReport
	var sig int32
	if err := unix.Prctl(unix.PR_GET_PDEATHSIG, uintptr(unsafe.Pointer(&sig)), 0, 0, 0); err == nil {
		r.PDeathSig = int(sig)
	}
	r.NoNewPrivs, _ = unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	var lim unix.Rlimit
	_ = unix.Getrlimit(unix.RLIMIT_NOFILE, &lim)
	r.NoFile = lim.Cur
	_ = unix.Getrlimit(unix.RLIMIT_CORE, &lim)
	r.Core = lim.Cur
	raw, _ := unix.Getpriority(unix.PRIO_PROCESS, 0)
	r.Nice = niceOf(raw)
	r.OwnGroup = unix.Getpgrp() == unix.Getpid()
	r.Cwd, _ = os.Getwd()
	r.Env = os.Environ()
	var null, st unix.Stat_t
	_ = unix.Stat(os.DevNull, &null)
	for fd, dst := range map[int]*bool{0: &r.StdinNull, 1: &r.StdoutNull} {
		if unix.Fstat(fd, &st) == nil {
			*dst = st.Rdev == null.Rdev && st.Mode&unix.S_IFMT == unix.S_IFCHR
		}
	}
	for fd, dst := range map[int]*bool{3: &r.FD3Socket, 4: &r.FD4Socket} {
		if unix.Fstat(fd, &st) == nil {
			*dst = st.Mode&unix.S_IFMT == unix.S_IFSOCK
		}
	}
	if f, err := os.Open(argv[0]); err != nil {
		r.Outside = errName(err)
	} else {
		f.Close()
	}
	if err := os.WriteFile(filepath.Join(argv[1], "written"), []byte("ok"), 0o600); err != nil {
		r.Inside = errName(err)
	}
	if err := unix.Kill(os.Getppid(), 0); err != nil {
		r.Signal = errName(err)
	}
	b, _ := json.Marshal(r)
	os.Stderr.Write(append(b, '\n'))
	return 0
}

func errName(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return unix.ErrnoName(errno)
	}
	return err.Error()
}

type run struct {
	code   int
	stderr string
	report *probeReport
}

// runTrampoline starts the test binary as the trampoline with two
// socketpairs on descriptors 3 and 4 (unless bare) and the probe as the
// plugin, the way the server does.
func runTrampoline(t *testing.T, p *Profile, env []string, bare bool, argv ...string) run {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) == 0 {
		argv = []string{self, "probe", "/nonexistent", p.Dir}
	}
	args, err := Args(p, argv...)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, args...)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if !bare {
		for i := 0; i < 2; i++ {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			mine, theirs := os.NewFile(uintptr(fds[0]), "core"), os.NewFile(uintptr(fds[1]), "plugin")
			defer mine.Close()
			defer theirs.Close()
			cmd.ExtraFiles = append(cmd.ExtraFiles, theirs)
		}
	}
	err = cmd.Run()
	r := run{stderr: stderr.String()}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(r.stderr, "\n") {
		if strings.HasPrefix(line, `{"pdeathsig"`) {
			r.report = &probeReport{}
			if err := json.Unmarshal([]byte(line), r.report); err != nil {
				t.Fatal(err)
			}
		}
	}
	return r
}

func sandboxProfile(t *testing.T) *Profile {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	return &Profile{ID: "acme_probe", ParentPID: os.Getpid(), Dir: data, Nice: 5, NoFile: 256,
		Landlock:        &Landlock{ReadExec: []string{filepath.Dir(self)}, ReadWrite: []string{data}, Read: []string{"/etc/ssl", "/nonexistent-ca"}},
		RequireLandlock: true}
}

func requireLandlock(t *testing.T) {
	t.Helper()
	if LandlockABI() == 0 {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("this kernel has no Landlock (5.13+ with landlock in the LSM list); GPUWORKS_TEST_STRICT=1 requires it")
		}
		t.Skip("this kernel has no Landlock (5.13+ with landlock in the LSM list)")
	}
}

// The plugin runs with every attribute of the profile, inherited across
// execve: parent-death signal, no_new_privs, its own process group, the
// limits and nice, the working directory, only the allowed environment,
// /dev/null on stdin and stdout, its sockets on 3 and 4, and a Landlock
// domain that denies a path outside the sandbox while the data directory
// stays writable.
func TestTrampolineAppliesTheProfile(t *testing.T) {
	requireLandlock(t)
	p := sandboxProfile(t)
	secret := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(secret, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	env := append(Environ(os.Environ(), p.Dir, p.Dir, 1), "GPUWORKS_SECRET=leak")
	r := runTrampoline(t, p, env, false, self, "probe", secret, p.Dir)
	if r.code != 0 || r.report == nil {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	got := r.report
	if got.PDeathSig != int(unix.SIGKILL) || got.NoNewPrivs != 1 || !got.OwnGroup {
		t.Errorf("pdeathsig=%d no_new_privs=%d own_group=%v", got.PDeathSig, got.NoNewPrivs, got.OwnGroup)
	}
	if got.NoFile != 256 || got.Core != 0 || got.Nice < 5 {
		t.Errorf("nofile=%d core=%d nice=%d", got.NoFile, got.Core, got.Nice)
	}
	if got.Cwd != p.Dir || !got.StdinNull || !got.StdoutNull || !got.FD3Socket || !got.FD4Socket {
		t.Errorf("cwd=%q stdin_null=%v stdout_null=%v fd3=%v fd4=%v", got.Cwd, got.StdinNull, got.StdoutNull, got.FD3Socket, got.FD4Socket)
	}
	for _, kv := range got.Env {
		if strings.HasPrefix(kv, "GPUWORKS_") {
			t.Errorf("the plugin saw %s", kv)
		}
	}
	if got.Outside != "EACCES" {
		t.Errorf("opening %s outside the sandbox: %q, want EACCES", secret, got.Outside)
	}
	if got.Inside != "" {
		t.Errorf("writing in the data directory: %s", got.Inside)
	}
	if b, err := os.ReadFile(filepath.Join(p.Dir, "written")); err != nil || string(b) != "ok" {
		t.Errorf("the data directory write did not land: %v", err)
	}
	if LandlockABI() >= 6 && got.Signal != "EPERM" {
		t.Errorf("signalling the server from the sandbox: %q, want EPERM (Landlock signal scope)", got.Signal)
	}
}

// Without Landlock the trampoline refuses a profile that requires it, and
// names the code; a profile that only asks for it runs unsandboxed.
func TestTrampolineRefusesWithoutRequiredLandlock(t *testing.T) {
	p := sandboxProfile(t)
	env := append(os.Environ(), "PLUGINEXEC_TEST_NO_LANDLOCK=1")
	r := runTrampoline(t, p, env, false)
	if r.code != ExitIsolationUnavailable || !strings.Contains(r.stderr, IsolationCode) || r.report != nil {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	p.RequireLandlock = false
	r = runTrampoline(t, p, env, false)
	if r.code != 0 || r.report == nil || !strings.Contains(r.stderr, "without a filesystem sandbox") {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
}

// The trampoline never starts a plugin it cannot set up as promised.
func TestTrampolineFailures(t *testing.T) {
	t.Run("parent gone", func(t *testing.T) {
		p := sandboxProfile(t)
		p.ParentPID = os.Getpid() + 100000
		if r := runTrampoline(t, p, nil, false); r.code != ExitSetupFailed || r.report != nil {
			t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
		}
	})
	t.Run("no sockets", func(t *testing.T) {
		p := sandboxProfile(t)
		if r := runTrampoline(t, p, nil, true); r.code != ExitSetupFailed || !strings.Contains(r.stderr, "descriptor 3") {
			t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
		}
	})
	t.Run("missing dir", func(t *testing.T) {
		p := sandboxProfile(t)
		p.Dir = filepath.Join(p.Dir, "missing")
		if r := runTrampoline(t, p, nil, false); r.code != ExitSetupFailed || !strings.Contains(r.stderr, "working directory") {
			t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
		}
	})
	t.Run("no binary", func(t *testing.T) {
		p := sandboxProfile(t)
		p.Landlock, p.RequireLandlock = nil, false
		if r := runTrampoline(t, p, nil, false, "/nonexistent/plugin", "serve"); r.code != ExitExecFailed {
			t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
		}
	})
	t.Run("usage", func(t *testing.T) {
		if code := Main([]string{"--profile"}); code != ExitUsage {
			t.Fatalf("exit %d", code)
		}
	})
}
