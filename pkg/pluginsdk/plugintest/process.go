package plugintest

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// closeWait is how long Close gives a plugin process to exit on its own
// once the server's ends of its connections closed, before killing it.
const closeWait = 2 * time.Second

// Process is a plugin binary that StartProcess runs the way the server
// does, without the server's sandbox: descriptors 3 and 4 are the serve
// and host connections, and the environment holds only what the server
// gives a plugin.
type Process struct {
	// Path is the binary; Args follow it ("serve" when empty).
	Path string
	Args []string
	// ID and Version are the package's, as Request names them.
	ID      string
	Version string
	// Dir is the working and home directory, the plugin's data directory;
	// empty is a new temporary directory.
	Dir string
	// Env is added to the plugin's environment, for a test's own switches;
	// the server adds nothing of the kind.
	Env []string
	// Stderr receives the plugin's log; nil discards it.
	Stderr io.Writer
}

// StartProcess starts p's binary on two socketpairs and returns the harness
// playing the server for it. It does not initialize the plugin; call
// Initialize. Wait returns nil once the process exited with status 0, and
// the *exec.ExitError otherwise. The process is killed with its group at
// the end of the test if it still runs.
func StartProcess(t testing.TB, p Process) *Harness {
	t.Helper()
	coreServe, pluginServe, err := filePair()
	if err != nil {
		t.Fatal(err)
	}
	coreHost, pluginHost, err := filePair()
	if err != nil {
		_ = coreServe.Close()
		_ = pluginServe.Close()
		t.Fatal(err)
	}
	h := newHarness(t, pluginsdk.Config{ID: p.ID, Version: p.Version}, coreServe, coreHost)
	cmd := exec.Command(p.Path, args(p.Args)...)
	cmd.Dir = p.Dir
	if cmd.Dir == "" {
		cmd.Dir = t.TempDir()
	}
	cmd.Env = append(environ(cmd.Dir, t.TempDir()), p.Env...)
	cmd.Stderr = p.Stderr // stdin and stdout stay nil: /dev/null
	cmd.ExtraFiles = []*os.File{pluginServe, pluginHost}
	cmd.SysProcAttr = sysProcAttr()

	started := make(chan error, 1)
	go func() {
		// The parent-death signal fires when the thread that started the
		// child exits, so this goroutine keeps its thread until the child
		// ended, as the server does.
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		h.done <- cmd.Wait()
	}()
	err = <-started
	_ = pluginServe.Close()
	_ = pluginHost.Close()
	if err != nil {
		_ = coreServe.Close()
		_ = coreHost.Close()
		t.Fatalf("plugintest: starting %s: %v", p.Path, err)
	}
	h.kill = func() error { return killGroup(cmd.Process) }
	h.connect()
	return h
}

func args(a []string) []string {
	if len(a) == 0 {
		return []string{"serve"}
	}
	return a
}

// environ is what the server gives every plugin: its search path and time
// zone, HOME at the data directory, TMPDIR, LANG and the gwx/1 variables.
func environ(home, tmp string) []string {
	var out []string
	for _, name := range []string{"PATH", "TZ"} {
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}
	return append(out, "HOME="+home, "TMPDIR="+tmp, "LANG=C.UTF-8",
		extapi.EnvProtocol+"="+strconv.Itoa(extapi.ProtocolMajor),
		extapi.EnvServeFD+"=3", extapi.EnvHostFD+"=4")
}
