//go:build multiproc

package l2

// `gpuworks install` and `gpuworks upgrade` are what a deployment actually runs,
// and both of them act on a host rather than inside a request: they copy a
// binary, start a detached service, take a snapshot, and put it back when the
// replacement does not come up. This file drives them the way an operator does —
// real binaries built with different release ids, a real download site on
// loopback, a real server process — and checks the states they promise: a
// service that answers, the password it was handed, a snapshot to go back to,
// and an audit line either way.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// controlPlanePassword is handed to install on the command line and must come
// back out of <data>/initial-credentials, which is the only place the server
// reads it from.
const controlPlanePassword = "l2-control-plane-password"

// toolTimeout bounds one install or upgrade. The readiness timeout inside them
// is a minute, and a rollback waits it out, so this is deliberately generous.
const toolTimeout = 4 * time.Minute

// releaseSite publishes the two things an upgrade reads, in the shape
// scripts/package_release.sh writes them onto the download site.
type releaseSite struct {
	*httptest.Server

	releaseID string
	archive   []byte
	manifest  []byte
}

func newReleaseSite(t *testing.T) *releaseSite {
	t.Helper()
	s := &releaseSite{}
	mux := http.NewServeMux()
	mux.HandleFunc("/data/release.json", func(w http.ResponseWriter, _ *http.Request) {
		if s.manifest == nil {
			http.NotFound(w, nil)
			return
		}
		_, _ = w.Write(s.manifest)
	})
	mux.HandleFunc("/downloads/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/downloads/")
		if s.archive == nil || name != s.assetName() {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(s.archive)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *releaseSite) assetName() string {
	return fmt.Sprintf("gpuworks-%s-%s-%s.tar.gz", s.releaseID, runtime.GOOS, runtime.GOARCH)
}

// publish offers one release, described by the same fields the real site writes:
// a size and a sha256 the download is checked against. The release is a
// packaged build for this platform, so the document is published from the
// program's bytes.
func (s *releaseSite) publish(t *testing.T, releaseID, binaryPath string) {
	t.Helper()
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read %s: %v", binaryPath, err)
	}
	s.releaseID = releaseID
	s.archive = releasePackage(t, releaseID, binary)
	// The site's own arch key is the platform card, which uses the macOS names
	// for darwin; goarch carries the exact Go target.
	arch := runtime.GOARCH
	if runtime.GOOS == "darwin" {
		arch = "macos-" + runtime.GOARCH
	}
	sum := sha256.Sum256(s.archive)
	manifest := map[string]any{
		"release_id": releaseID,
		"commit":     "deadbeef",
		"build_at":   "2026-03-04T05:06:07Z",
		"channel":    "preview",
		"files": []map[string]any{{
			"os": runtime.GOOS, "arch": arch, "goarch": runtime.GOARCH,
			"name": s.assetName(), "size": len(s.archive), "sha256": hex.EncodeToString(sum[:]),
		}},
		"checksums": "SHA256SUMS",
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode release manifest: %v", err)
	}
	s.manifest = payload
}

// releasePackage builds the artifact the site serves: a tar.gz holding the
// binary inside a directory named after the package, alongside INSTALL.txt.
func releasePackage(t *testing.T, releaseID string, binary []byte) []byte {
	t.Helper()
	pkg := fmt.Sprintf("gpuworks-%s-%s-%s", releaseID, runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writer := tar.NewWriter(gz)
	for _, member := range []struct {
		name string
		data []byte
		mode int64
	}{
		{name: pkg + "/gpuworks", data: binary, mode: 0o755},
		{name: pkg + "/INSTALL.txt", data: []byte("GPUWorks " + releaseID + "\n"), mode: 0o644},
	} {
		header := &tar.Header{Name: member.name, Mode: member.mode, Typeflag: tar.TypeReg,
			Size: int64(len(member.data))}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("tar header %s: %v", member.name, err)
		}
		if _, err := writer.Write(member.data); err != nil {
			t.Fatalf("tar body %s: %v", member.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// buildServerRelease builds the control plane with a release id, which is what
// the download site names and what the installed binary reports.
func buildServerRelease(t *testing.T, releaseID string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "gpuworks-"+releaseID)
	args := []string{"build", "-trimpath",
		"-ldflags", "-X gpuworks.ai/gpuworks/internal/version.Version=" + releaseID}
	if os.Getenv("GOCOVERDIR") != "" {
		args = append(args, "-cover", "-coverpkg=./cmd/...,./internal/...,./pkg/...")
	}
	args = append(args, "-o", out, "./cmd/gpuworks")
	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build release %s: %v\n%s", releaseID, err, output)
	}
	return out
}

// brokenBuild returns a file that is packed and installed like a release but
// cannot be executed: the kernel refuses it, so the replacement never starts.
func brokenBuild(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gpuworks-v2.0.0")
	if err := os.WriteFile(path, []byte("this build cannot be executed\n"), 0o755); err != nil {
		t.Fatalf("write the broken build: %v", err)
	}
	return path
}

// controlPlane is one installation, as an operator would see it.
type controlPlane struct {
	dir     string
	dataDir string
	bin     string
	base    string
}

// installedConfig mirrors <dir>/server.yaml. It is read with literal keys
// because that file is what the server and a later binary read.
type installedConfig struct {
	Listen struct {
		Console []string `yaml:"console"`
	} `yaml:"listen"`
	DataDir string `yaml:"data_dir"`
	Log     struct {
		File string `yaml:"file"`
	} `yaml:"log"`
}

// historyLine mirrors one line of <data>/install-history.jsonl.
type historyLine struct {
	Action            string `json:"action"`
	Result            string `json:"result"`
	ReleaseID         string `json:"release_id"`
	PreviousReleaseID string `json:"previous_release_id"`
	Backup            string `json:"backup"`
	Mode              string `json:"mode"`
	At                string `json:"at"`
	Error             string `json:"error"`
}

// installControlPlane installs a release the way the plan's deployment does:
// detached (no systemd), unattended, with a password on the command line, and
// with no service account so the test never touches the host's accounts.
func installControlPlane(t *testing.T, releaseID string) controlPlane {
	t.Helper()
	source := buildServerRelease(t, releaseID)
	dir := t.TempDir()
	addr := "127.0.0.1:" + freePort(t)
	runTool(t, source, "install", "--dir", dir, "--addr", addr, "--no-systemd",
		"--service-user", "root", "--quiet", "--password", controlPlanePassword)

	fx := controlPlane{dir: dir, dataDir: filepath.Join(dir, "data"), bin: filepath.Join(dir, "gpuworks"),
		base: "http://" + addr}
	t.Cleanup(func() { stopInstalledControlPlane(t, dir) })

	if !isExecutable(fx.bin) {
		t.Fatalf("install did not leave an executable at %s", fx.bin)
	}
	if got := runTool(t, fx.bin, "version"); !strings.Contains(got, releaseID) {
		t.Fatalf("the installed binary reports %q, want %s", strings.TrimSpace(got), releaseID)
	}
	// The unit file must never carry the password; this file is the hand-off.
	record := readCredentials(t, filepath.Join(fx.dataDir, "initial-credentials"))
	if record.User != "admin" || record.Password != controlPlanePassword {
		t.Fatalf("initial-credentials = %+v, want the password install was given", record)
	}
	// The service is up, and it reports the release it was built from, which is
	// the only end-to-end proof that the running process is this binary.
	assertHealthVersion(t, fx.base, releaseID)
	// The recorded password is the one the server accepts.
	loginWithPassword(t, fx.base, record.Password)
	// The pid file names a process running the installed binary.
	pid := readPidFile(t, filepath.Join(dir, "gpuworks.pid"))
	if argv := cmdlineOf(t, pid); len(argv) == 0 || argv[0] != fx.bin {
		t.Fatalf("pid %d runs %q, want %s", pid, argv, fx.bin)
	}

	cfg := readInstalledConfig(t, dir)
	if len(cfg.Listen.Console) != 1 || cfg.Listen.Console[0] != addr || cfg.DataDir != fx.dataDir {
		t.Fatalf("server.yaml = %+v", cfg)
	}
	// The process runs from server.yaml alone, behind the supervisor that
	// stands in for systemd, and nothing else records the installation.
	if argv := cmdlineOf(t, pid); strings.Join(argv[1:], " ") != "serve --supervise --config "+filepath.Join(dir, "server.yaml") {
		t.Fatalf("pid %d runs %q", pid, argv)
	}
	if _, err := os.Stat(filepath.Join(dir, "install.json")); !os.IsNotExist(err) {
		t.Fatalf("install.json must not exist: %v", err)
	}
	if entries := readInstalledHistory(t, dir); len(entries) != 1 ||
		entries[0].Action != "install" || entries[0].Result != "ok" || entries[0].ReleaseID != releaseID {
		t.Fatalf("install history = %+v", entries)
	}
	return fx
}

func TestInstallServesAndUpgradeReplacesTheRelease(t *testing.T) {
	site := newReleaseSite(t)
	site.publish(t, "v1.2.4", buildServerRelease(t, "v1.2.4"))
	fx := installControlPlane(t, "v1.2.3")

	// --check reports the update and touches nothing: no snapshot, no change to
	// the recorded release.
	check := runTool(t, fx.bin, "upgrade", "--dir", fx.dir, "--base-url", site.URL, "--check")
	if !strings.Contains(check, "update available: v1.2.3 -> v1.2.4") {
		t.Fatalf("check output:\n%s", check)
	}
	if got := runTool(t, fx.bin, "version"); !strings.Contains(got, "v1.2.3") {
		t.Fatalf("--check replaced the binary: %s", got)
	}
	if backups, _ := filepath.Glob(filepath.Join(fx.dir, "backups", "*.tar.gz")); len(backups) != 0 {
		t.Fatalf("--check took a snapshot: %v", backups)
	}

	// --quiet narrates nothing on success; a failure still prints to stderr.
	out := runTool(t, fx.bin, "upgrade", "--dir", fx.dir, "--base-url", site.URL, "--quiet")
	if strings.TrimSpace(out) != "" {
		t.Fatalf("a quiet upgrade printed:\n%s", out)
	}

	// The running service is the new release, not the file the old process was
	// started from.
	assertHealthVersion(t, fx.base, "v1.2.4")
	if got := runTool(t, fx.bin, "version"); !strings.Contains(got, "v1.2.4") {
		t.Fatalf("the installed binary reports %q, want v1.2.4", strings.TrimSpace(got))
	}
	// The snapshot holds what a rollback needs, and is named after the version
	// it came from.
	backup := singleBackup(t, fx.dir)
	if !strings.HasPrefix(filepath.Base(backup), "v1.2.3-") {
		t.Fatalf("the snapshot is named %q, want the version it was taken from", filepath.Base(backup))
	}
	for _, member := range []string{"bin/gpuworks", "data/gpuworks.db", "data/master.key"} {
		if !contains(archiveMembers(t, backup), member) {
			t.Fatalf("the snapshot is missing %s (has %v)", member, archiveMembers(t, backup))
		}
	}

	entries := readInstalledHistory(t, fx.dir)
	if len(entries) != 2 {
		t.Fatalf("install history = %+v", entries)
	}
	last := entries[1]
	if last.Action != "upgrade" || last.Result != "ok" || last.ReleaseID != "v1.2.4" ||
		last.PreviousReleaseID != "v1.2.3" || last.Backup == "" || last.At == "" {
		t.Fatalf("the upgrade was not recorded: %+v", last)
	}
}

func TestUpgradeRollsBackWhenTheReleaseCannotRun(t *testing.T) {
	site := newReleaseSite(t)
	site.publish(t, "v2.0.0", brokenBuild(t))
	fx := installControlPlane(t, "v1.2.3")

	out, err := runToolErr(t, fx.bin, "upgrade", "--dir", fx.dir, "--base-url", site.URL, "--quiet")
	if err == nil {
		t.Fatalf("an upgrade to a build that cannot run reported success:\n%s", out)
	}
	if !strings.Contains(out, "rolled back") {
		t.Fatalf("the failure does not say what happened:\n%s", out)
	}

	// The previous version is back in charge: on disk, and serving.
	if got := runTool(t, fx.bin, "version"); !strings.Contains(got, "v1.2.3") {
		t.Fatalf("the installed binary reports %q, want the restored v1.2.3", strings.TrimSpace(got))
	}
	assertHealthVersion(t, fx.base, "v1.2.3")
	assertRollbackRecorded(t, fx, "v2.0.0")
}

func TestUpgradeRollsBackWhenTheReleaseNeverAnswers(t *testing.T) {
	// A real program that starts and exits without serving: the binary is
	// replaceable and startable, so only the readiness probe rejects it.
	if !isExecutable("/bin/true") {
		t.Skip("no /bin/true to build the fixture from")
	}
	site := newReleaseSite(t)
	site.publish(t, "v2.0.0", "/bin/true")
	fx := installControlPlane(t, "v1.2.3")

	out, err := runToolErr(t, fx.bin, "upgrade", "--dir", fx.dir, "--base-url", site.URL, "--quiet")
	if err == nil {
		t.Fatalf("an upgrade to a build that never answers reported success:\n%s", out)
	}
	// /bin/true cannot supervise itself: report the exited process, rather
	// than exhausting the readiness budget. The whole command also compresses
	// a binary, restores a snapshot and starts the old server; its duration
	// does not measure the failed release's probe. The deterministic probe
	// count is guarded by TestWaitHealthyStopsWhenAnUnsupervisedServerIsGone.
	if !strings.Contains(out, "the server is no longer running") {
		t.Fatalf("the exited release was not identified by the readiness probe:\n%s", out)
	}
	assertHealthVersion(t, fx.base, "v1.2.3")
	assertRollbackRecorded(t, fx, "v2.0.0")
}

// assertRollbackRecorded checks the half of a failed upgrade an operator depends
// on: the previous version is running and the attempt is in the audit log.
func assertRollbackRecorded(t *testing.T, fx controlPlane, attempted string) {
	t.Helper()
	backup := singleBackup(t, fx.dir)
	entries := readInstalledHistory(t, fx.dir)
	if len(entries) != 2 {
		t.Fatalf("install history = %+v", entries)
	}
	last := entries[1]
	if last.Action != "upgrade" || last.Result != "failed" || last.ReleaseID != attempted ||
		last.PreviousReleaseID != "v1.2.3" || last.Backup != backup || last.Error == "" {
		t.Fatalf("the failed upgrade was not recorded: %+v", last)
	}
}

// runTool runs an installed subcommand, failing the test on a non-zero exit.
func runTool(t *testing.T, bin string, args ...string) string {
	t.Helper()
	out, err := runToolErr(t, bin, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return out
}

func runToolErr(t *testing.T, bin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(out), err
}

type healthBody struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// assertHealthVersion waits for the service to answer and to report the release
// it is running.
func assertHealthVersion(t *testing.T, base, wantVersion string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := "no answer"
	for time.Now().Before(deadline) {
		resp, err := processHTTPClient.Get(base + "/healthz")
		if err == nil {
			var body healthBody
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && body.Status == "ok" {
				if body.Version == wantVersion {
					return
				}
				last = fmt.Sprintf("the service reports %q, want %q", body.Version, wantVersion)
			} else {
				last = fmt.Sprintf("status %d, body %+v", resp.StatusCode, body)
			}
		} else {
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s/healthz never reported %s: %s", base, wantVersion, last)
}

type credentialsRecord struct {
	User      string `json:"user"`
	Password  string `json:"password"`
	CreatedAt string `json:"created_at"`
}

func readCredentials(t *testing.T, path string) credentialsRecord {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s is %v, want 0600 (the standalone server reads its private credentials)", path, info.Mode().Perm())
	}
	var record credentialsRecord
	if err := json.Unmarshal(readFileAt(t, path), &record); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if record.CreatedAt == "" {
		t.Fatalf("%s does not record when it was written", path)
	}
	return record
}

func readInstalledConfig(t *testing.T, dir string) installedConfig {
	t.Helper()
	path := filepath.Join(dir, "server.yaml")
	var cfg installedConfig
	if err := yaml.Unmarshal(readFileAt(t, path), &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg
}

func readInstalledHistory(t *testing.T, dir string) []historyLine {
	t.Helper()
	path := filepath.Join(dir, "data", "install-history.jsonl")
	var entries []historyLine
	for _, line := range strings.Split(strings.TrimSpace(string(readFileAt(t, path))), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry historyLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parse a history line in %s: %v\n%s", path, err, line)
		}
		entries = append(entries, entry)
	}
	return entries
}

// singleBackup returns the one snapshot in the backups directory.
func singleBackup(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "backups", "*.tar.gz"))
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("backups = %v, want exactly one", matches)
	}
	return matches[0]
}

func archiveMembers(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		names = append(names, header.Name)
	}
}

func readPidFile(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(string(readFileAt(t, path))))
	if err != nil || pid <= 0 {
		t.Fatalf("%s does not name a pid: %v", path, err)
	}
	return pid
}

func cmdlineOf(t *testing.T, pid int) []string {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Skipf("cannot read the command line of pid %d: %v", pid, err)
	}
	var argv []string
	for _, field := range strings.Split(string(raw), "\x00") {
		if field != "" {
			argv = append(argv, field)
		}
	}
	return argv
}

// stopInstalledControlPlane stops the detached server so the test leaves no
// process behind.
func stopInstalledControlPlane(t *testing.T, dir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "gpuworks.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("pid %d did not stop on SIGTERM; killing it", pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func readFileAt(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0o111 != 0
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// An address saved into server.yaml (by the console or by hand) takes effect
// when `gpuworks restart` brings the service back.
func TestRestartAppliesAnEditedServerConfig(t *testing.T) {
	fx := installControlPlane(t, "v1.2.3")
	path := filepath.Join(fx.dir, "server.yaml")
	raw := string(readFileAt(t, path))
	oldAddr := strings.TrimPrefix(fx.base, "http://")
	newAddr := "127.0.0.1:" + freePort(t)
	if !strings.Contains(raw, oldAddr) {
		t.Fatalf("server.yaml does not bind %s:\n%s", oldAddr, raw)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(raw, oldAddr, newAddr)), 0o664); err != nil {
		t.Fatal(err)
	}
	out := runTool(t, fx.bin, "restart", "--dir", fx.dir)
	if !strings.Contains(out, "restarted (nohup)") {
		t.Fatalf("restart output:\n%s", out)
	}
	assertHealthVersion(t, "http://"+newAddr, "v1.2.3")
	if res, err := http.Get(fx.base + "/healthz"); err == nil {
		res.Body.Close()
		t.Fatalf("the old address %s still answers after the restart", oldAddr)
	}
}
