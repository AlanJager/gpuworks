package offlinepkg

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAgent is an "agent binary" for one platform: install.sh copies it into
// place and runs it (configure), and it records which platform it is.
func fakeAgent(platform string) []byte {
	return []byte("#!/bin/sh\necho \"" + platform + " $1\" >> \"$CALLS\"\n[ \"$1\" = configure ] && cat >/dev/null\nexit 0\n")
}

func agents(missing ...string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		for _, m := range missing {
			if m == name {
				return nil, fs.ErrNotExist
			}
		}
		return fakeAgent(name), nil
	}
}

// runPackage runs the package as a node whose uname says sysname/machine.
func runPackage(t *testing.T, pkg []byte, sysname, machine string, args ...string) (string, string, error) {
	t.Helper()
	bin, work := t.TempDir(), t.TempDir()
	for _, tool := range []string{"sh", "cat", "cp", "sed", "mkdir", "chmod", "mv", "printf", "echo", "nohup", "tr", "awk", "tail", "head", "gzip", "mktemp", "rm", "id", "sleep"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s", tool)
		}
		if err := os.Symlink(real, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	uname := "#!/bin/sh\ncase \"$1\" in -s) echo " + sysname + " ;; -m) echo " + machine + " ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "uname"), []byte(uname), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "sw_vers"), []byte("#!/bin/sh\necho 15.4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(work, FileName)
	if err := os.WriteFile(file, pkg, 0o644); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(work, "calls")
	cmd := exec.Command(filepath.Join(bin, "sh"), append([]string{file}, args...)...)
	cmd.Env = []string{"PATH=" + bin, "HOME=" + work, "CALLS=" + calls, "TMPDIR=" + work, "GPUWORKS_NO_SYSTEMD=1"}
	out, err := packageOutput(cmd)
	recorded, _ := os.ReadFile(calls)
	return string(out), string(recorded), err
}

func TestPackageInstallsTheAgentOfTheNodesPlatform(t *testing.T) {
	pkg, err := Build("v1.2.3", agents())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sysname, machine, want string }{
		{"Linux", "x86_64", "gpuworks-agent-linux-amd64"},
		{"Linux", "aarch64", "gpuworks-agent-linux-arm64"},
		{"Darwin", "arm64", "gpuworks-agent-darwin-arm64"},
	} {
		t.Run(tc.machine, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "agent")
			out, calls, err := runPackage(t, pkg, tc.sysname, tc.machine, "--token", "t", "--connect-mode", "server_dial", "--listen", "0.0.0.0:6006", "--install-dir", dir, "--no-sudo")
			if err != nil {
				t.Fatalf("package: %v\n%s", err, out)
			}
			if !strings.Contains(calls, tc.want+" configure") || strings.Count(calls, "configure") != 1 {
				t.Fatalf("the package ran the wrong agent:\n%s\n%s", calls, out)
			}
			installed, err := os.ReadFile(filepath.Join(dir, "gpuworks-agent"))
			if err != nil || string(installed) != string(fakeAgent(tc.want)) {
				t.Fatalf("installed agent = %q, %v", installed, err)
			}
		})
	}
}

func TestPackageRefusesAPlatformItDoesNotCarry(t *testing.T) {
	pkg, err := Build("v1", agents("gpuworks-agent-linux-arm64"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sysname, machine string }{{"Linux", "aarch64"}, {"Darwin", "x86_64"}, {"FreeBSD", "amd64"}} {
		out, calls, err := runPackage(t, pkg, tc.sysname, tc.machine, "--token", "t")
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 || !strings.Contains(out, "this package has no agent for "+tc.sysname) || calls != "" {
			t.Fatalf("%s/%s: %v\n%s", tc.sysname, tc.machine, err, out)
		}
		if !strings.Contains(out, "Linux x86_64, macOS (Apple silicon)") {
			t.Fatalf("the refusal does not say what the package carries:\n%s", out)
		}
	}
}

// install.sh's own refusals reach the operator with its exit status, and the
// temporary directory is removed either way.
func TestPackagePassesOnTheInstallersFailure(t *testing.T) {
	pkg, err := Build("v1", agents())
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runPackage(t, pkg, "Linux", "x86_64", "--connect-mode", "server_dial")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || !strings.Contains(out, "--token is required") {
		t.Fatalf("package = %v\n%s", err, out)
	}
}

func TestBuildWithoutAgentsFails(t *testing.T) {
	if _, err := Build("v1", func(string) ([]byte, error) { return nil, fs.ErrNotExist }); err != ErrNoAgents {
		t.Fatalf("Build = %v", err)
	}
	if _, err := Build("v1", func(string) ([]byte, error) { return nil, io.ErrUnexpectedEOF }); err == nil {
		t.Fatal("a read error was packaged over")
	}
}

func TestServerServesThePackageAndItsChecksum(t *testing.T) {
	s := NewServer(t.TempDir(), "v1", agents())
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	pkg := get("/dl/" + FileName)
	if pkg.Code != http.StatusOK || pkg.Header().Get("Content-Disposition") != `attachment; filename="gpuworks-agent.run"` || !strings.HasPrefix(pkg.Body.String(), "#!/bin/sh\n") {
		t.Fatalf("package response %d %v", pkg.Code, pkg.Header())
	}
	sum := sha256.Sum256(pkg.Body.Bytes())
	if got := get("/dl/" + ChecksumName).Body.String(); got != hex.EncodeToString(sum[:])+"  "+FileName+"\n" {
		t.Fatalf("checksum = %q", got)
	}
	if !Handles("/dl/"+FileName) || !Handles("/dl/"+ChecksumName) || Handles("/dl/gpuworks-agent-linux-amd64") {
		t.Fatal("Handles answers the wrong paths")
	}
	empty := NewServer(t.TempDir(), "v1", func(string) ([]byte, error) { return nil, fs.ErrNotExist })
	rec := httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dl/"+FileName, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a server without agents answered %d", rec.Code)
	}
}
