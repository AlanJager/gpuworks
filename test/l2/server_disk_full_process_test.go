//go:build multiproc && linux

package l2

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// diskFullRig is a real server process whose data directory sits on a 64 MiB
// tmpfs mounted in a user and mount namespace of its own, so the test can fill
// the disk for real: SQLite, the log file and the diagnostics all meet the
// kernel's ENOSPC, not a stub. A sleeping holder keeps the namespace alive
// while servers are stopped and started inside it with nsenter.
type diskFullRig struct {
	t       *testing.T
	holder  *exec.Cmd
	mount   string
	data    string
	stderr  string
	base    string
	addr    string
	server  *exec.Cmd
	exited  chan error
	binary  string
	scratch string
}

func requireUserNamespaces(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"unshare", "nsenter", "dd"} {
		if _, err := exec.LookPath(tool); err != nil {
			skipOrFailDiskFull(t, fmt.Sprintf("%s unavailable: %v", tool, err))
		}
	}
	if out, err := exec.Command("unshare", "-rm", "sh", "-c", `mount -t tmpfs -o size=1m tmpfs "$0"`, t.TempDir()).CombinedOutput(); err != nil {
		skipOrFailDiskFull(t, fmt.Sprintf("unprivileged user namespaces cannot mount a tmpfs here: %v %s", err, out))
	}
}

func skipOrFailDiskFull(t *testing.T, why string) {
	t.Helper()
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf("%s (strict mode)", why)
	}
	t.Skip(why)
}

func newDiskFullRig(t *testing.T) *diskFullRig {
	t.Helper()
	scratch := t.TempDir()
	r := &diskFullRig{t: t, scratch: scratch, mount: filepath.Join(scratch, "fs"), stderr: filepath.Join(scratch, "server.stderr"), binary: buildBinary(t, "./cmd/gpuworks")}
	r.data = filepath.Join(r.mount, "data")
	if err := os.MkdirAll(r.mount, 0o755); err != nil {
		t.Fatal(err)
	}
	r.holder = exec.Command("unshare", "-rm", "--propagation", "private", "sh", "-c",
		`mount -t tmpfs -o size=64m tmpfs "$0" && mkdir "$0/data" && echo ready && exec sleep 3600`, r.mount)
	out, err := r.holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	r.holder.Stderr = os.Stderr
	if err := r.holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		r.stop()
		_ = r.holder.Process.Kill()
		_ = r.holder.Wait()
	})
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "ready" {
		t.Fatalf("namespace holder did not mount its tmpfs: %q", line)
	}
	r.addr = "127.0.0.1:" + freePort(t)
	r.base = "http://" + r.addr
	return r
}

// in runs argv inside the rig's namespaces, where the tmpfs is mounted.
func (r *diskFullRig) in(argv ...string) *exec.Cmd {
	args := append([]string{"-t", fmt.Sprint(r.holder.Process.Pid), "-U", "-m", "--preserve-credentials"}, argv...)
	return exec.Command("nsenter", args...)
}

func (r *diskFullRig) sh(script string) string {
	r.t.Helper()
	out, _ := r.in("sh", "-c", script).CombinedOutput()
	return string(out)
}

func (r *diskFullRig) available() int64 {
	r.t.Helper()
	var n int64
	fmt.Sscan(strings.TrimSpace(r.sh(`df -B1 --output=avail "`+r.mount+`" | tail -1`)), &n)
	return n
}

// fill takes every byte the tmpfs has left, down to the last 1 KiB block.
func (r *diskFullRig) fill() {
	r.t.Helper()
	r.sh(fmt.Sprintf(`dd if=/dev/zero of="%[1]s/filler-$$-m" bs=1M 2>/dev/null; dd if=/dev/zero of="%[1]s/filler-$$-k" bs=1k 2>/dev/null; true`, r.mount))
	if got := r.available(); got != 0 {
		r.t.Fatalf("tmpfs still has %d bytes after filling it", got)
	}
}

func (r *diskFullRig) free() {
	r.t.Helper()
	r.sh(`rm -f "` + r.mount + `"/filler-*`)
	if r.available() < 32<<20 {
		r.t.Fatalf("freeing the fillers left only %d bytes", r.available())
	}
}

// start launches the server; ok reports whether it came up, else its exit.
func (r *diskFullRig) start() (bool, error) {
	r.t.Helper()
	stderr, err := os.OpenFile(r.stderr, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		r.t.Fatal(err)
	}
	cmd := r.in("env", "GPUWORKS_ADMIN_PASSWORD=multiproc-password", r.binary, "serve", "--data", r.data, "--addr", r.addr)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.server, r.exited = cmd, make(chan error, 1)
	go func() { r.exited <- cmd.Wait(); _ = stderr.Close() }()
	deadline := time.Now().Add(serverHealthTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-r.exited:
			r.server = nil
			return false, err
		default:
		}
		if resp, err := processHTTPClient.Get(r.base + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.t.Fatalf("server neither served /healthz nor exited:\n%s", r.stderrText())
	return false, nil
}

func (r *diskFullRig) stop() {
	if r.server == nil {
		return
	}
	_ = r.server.Process.Signal(os.Interrupt)
	select {
	case <-r.exited:
	case <-time.After(15 * time.Second):
		_ = r.server.Process.Kill()
		<-r.exited
	}
	r.server = nil
}

func (r *diskFullRig) stderrText() string {
	raw, _ := os.ReadFile(r.stderr)
	return string(raw)
}

type diskFullAnswer struct {
	status int
	Code   string         `json:"code"`
	Error  string         `json:"error"`
	Params map[string]any `json:"params"`
	ID     string         `json:"id"`
}

func (r *diskFullRig) post(token, path, body string) diskFullAnswer {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", r.base)
	resp, err := processHTTPClient.Do(req)
	if err != nil {
		r.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var a diskFullAnswer
	_ = json.NewDecoder(resp.Body).Decode(&a)
	a.status = resp.StatusCode
	return a
}

func (r *diskFullRig) registerModel(token string, i int) diskFullAnswer {
	return r.post(token, "/api/v1/models", fmt.Sprintf(`{"source":"huggingface","source_id":"org/m-%d","name":"m-%d","browser_metadata":true,"config_raw":"{\"model_type\":\"qwen2\"}","weight_bytes":1000}`, i, i))
}

// writeUntilRefused keeps registering models until the server refuses one: a
// full disk does not refuse the first write, since SQLite reuses the pages it
// already holds.
func (r *diskFullRig) writeUntilRefused(token string, from int) diskFullAnswer {
	r.t.Helper()
	for i := from; i < from+5000; i++ {
		if a := r.registerModel(token, i); a.status != http.StatusCreated {
			return a
		}
	}
	r.t.Fatal("5000 writes on a full disk all succeeded")
	return diskFullAnswer{}
}

func archiveFile(t *testing.T, archive, suffix string) []byte {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("%s has no %s: %v", archive, suffix, err)
		}
		if strings.HasSuffix(h.Name, suffix) {
			raw, _ := io.ReadAll(tr)
			return raw
		}
	}
}

func diskJSONAvailable(t *testing.T, raw []byte, kind string) int64 {
	t.Helper()
	var report struct {
		Dirs []struct {
			Kind      string `json:"kind"`
			Available int64  `json:"available_bytes"`
			Total     int64  `json:"total_bytes"`
		} `json:"dirs"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("disk.json %s: %v", raw, err)
	}
	for _, d := range report.Dirs {
		if d.Kind == kind && d.Total > 0 {
			return d.Available
		}
	}
	t.Fatalf("disk.json has no %s directory: %s", kind, raw)
	return 0
}

// TestProcessServerOnAFullDisk fills the disk under a running server's data
// directory for real and checks what an operator is left with: an answer
// that names the disk, a log that comes back by itself, a collection that
// still says what failed, an offline collection written elsewhere, and a
// start refused with the reason rather than a bare disk I/O error.
//
// go test -tags=multiproc -run TestProcessServerOnAFullDisk -timeout 10m ./test/l2/
func TestProcessServerOnAFullDisk(t *testing.T) {
	requireUserNamespaces(t)
	r := newDiskFullRig(t)
	if up, err := r.start(); !up {
		t.Fatalf("server did not start on an empty tmpfs: %v\n%s", err, r.stderrText())
	}
	token := login(t, r.base)
	if a := r.registerModel(token, 0); a.status != http.StatusCreated {
		t.Fatalf("baseline write = %d %+v", a.status, a)
	}

	// 1. A write on a full disk names it.
	r.fill()
	refused := r.writeUntilRefused(token, 1)
	if refused.status != http.StatusInsufficientStorage || refused.Code != "SERVER_STORAGE_FULL" {
		t.Fatalf("refusal = %d %+v, want 507 SERVER_STORAGE_FULL", refused.status, refused)
	}
	if refused.Params["path"] != r.data || refused.Params["available_bytes"] != float64(0) || refused.Params["total_bytes"] != float64(64<<20) {
		t.Fatalf("refusal params = %v, want the data directory with 0 of 64 MiB free", refused.Params)
	}
	// The log file gave up; stderr says so and keeps every record.
	if !strings.Contains(r.stderrText(), `msg="log file writes failed; using stderr until they succeed again"`) {
		t.Fatalf("stderr does not say the log file stopped:\n%s", r.stderrText())
	}

	// 2. A console collection on the full disk fails visibly: either the
	// request itself is refused, or the collection is listed as failed.
	b := r.post(token, "/api/v1/diagnostics/bundles", `{}`)
	switch b.status {
	case http.StatusInsufficientStorage:
		if b.Code != "SERVER_STORAGE_FULL" {
			t.Fatalf("collection refusal = %+v", b)
		}
	case http.StatusAccepted:
		deadline := time.Now().Add(20 * time.Second)
		for {
			req, _ := http.NewRequest(http.MethodGet, r.base+"/api/v1/diagnostics/bundles", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := processHTTPClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var list struct {
				Bundles []struct{ ID, State, Error string } `json:"bundles"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&list)
			resp.Body.Close()
			found := false
			for _, item := range list.Bundles {
				if item.ID == b.ID && item.State == "failed" && strings.Contains(item.Error, "no space left on device") {
					found = true
				}
			}
			if found {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("collection %s is not listed as failed for lack of space: %+v", b.ID, list.Bundles)
			}
			time.Sleep(200 * time.Millisecond)
		}
	default:
		t.Fatalf("collection on a full disk = %d %+v", b.status, b)
	}

	// 3. The offline collection works on the full disk, staged beside --out.
	out := filepath.Join(r.scratch, "offline.tar.gz")
	if raw, err := r.in(r.binary, "support-bundle", "--data", r.data, "--out", out).CombinedOutput(); err != nil {
		t.Fatalf("support-bundle on a full disk: %v\n%s", err, raw)
	}
	if got := diskJSONAvailable(t, archiveFile(t, out, "server/disk.json"), "data"); got != 0 {
		t.Fatalf("offline disk.json says %d bytes free, want 0", got)
	}

	// 4. Room again: writes succeed at once, and the log file comes back by
	// itself, with a line saying what it lost.
	r.free()
	if a := r.registerModel(token, 9000); a.status != http.StatusCreated {
		t.Fatalf("write after freeing space = %d %+v", a.status, a)
	}
	logFile := filepath.Join(r.data, "logs", "gpuworks-server.log")
	deadline := time.Now().Add(45 * time.Second)
	for !strings.Contains(r.sh(`cat "`+logFile+`"`), `msg="log file writes resumed"`) {
		if time.Now().After(deadline) {
			t.Fatalf("log file did not resume:\n%s", r.sh(`tail -c 2000 "`+logFile+`"`))
		}
		r.registerModel(token, int(time.Now().UnixNano()%1e9))
		time.Sleep(time.Second)
	}

	// 5. A start on a full disk exits and says the disk is why.
	r.stop()
	r.fill()
	before := len(r.stderrText())
	if up, _ := r.start(); up {
		t.Fatal("server started with 0 bytes free; the test no longer covers a refused start")
	}
	if tail := r.stderrText()[before:]; !strings.Contains(tail, `msg="server exited"`) || !strings.Contains(tail, "has 0 B free of 64.0 MiB") || !strings.Contains(tail, r.data) {
		t.Fatalf("exit on a full disk does not name the disk:\n%s", tail)
	}

	// 6. And starts again once there is room.
	r.free()
	if up, err := r.start(); !up {
		t.Fatalf("server did not start after freeing space: %v\n%s", err, r.stderrText())
	}
	token = login(t, r.base)
	if a := r.registerModel(token, 9999); a.status != http.StatusCreated {
		t.Fatalf("write after restart = %d %+v", a.status, a)
	}
}
