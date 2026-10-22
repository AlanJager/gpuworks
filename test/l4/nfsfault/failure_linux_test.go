//go:build linux && l4

package nfsfault

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/proto"
)

func runFailureRetry(t *testing.T, lab *nfsLab, mode string) {
	r := newCopyRun(t, lab, mode)
	partial := r.wait(t, 30*time.Second, func(st proto.ModelFileStatus) bool { return st.BytesDone > 1<<20 && st.BytesDone < st.BytesTotal/2 })
	t.Logf("partial copy before %s: %d/%d", mode, partial.BytesDone, partial.BytesTotal)
	var restore func()
	if mode == "disk_full" {
		cache := filepath.Dir(r.target)
		restore = func() { mustCommand(t, "mount", "-o", "remount,size=2G", cache) }
		defer restore()
		// Shrink only this private tmpfs quota after admission and partial writes.
		// This exercises a write-time ENOSPC, not just the free-space precheck.
		mustCommand(t, "mount", "-o", "remount,size=16M", cache)
	} else {
		source, err := os.Open(filepath.Join(r.source, "model.safetensors"))
		if err != nil {
			t.Fatal(err)
		}
		if err := source.Sync(); err != nil {
			t.Fatal(err)
		}
		// Evict only this clean fixture inode from the server's page cache. Never
		// write /proc/sys/vm/drop_caches; production caches remain untouched.
		err = unix.Fadvise(int(source.Fd()), 0, 0, unix.FADV_DONTNEED)
		_ = source.Close()
		if err != nil {
			t.Fatal(err)
		}
		restore = lab.fault(t, "eio")
		defer restore()
	}
	var failed proto.ModelFileStatus
	r.env.Eventually(t, 45*time.Second, func() bool {
		failed = r.observe(t)
		return failed.State == proto.MFFailed || failed.State == proto.MFInsufficientDisk
	})
	if mode == "disk_full" {
		if failed.State != proto.MFInsufficientDisk || !strings.Contains(strings.ToLower(failed.Message), "no space left") {
			t.Fatalf("write-time ENOSPC not propagated: %+v", failed)
		}
	} else if failed.State != proto.MFFailed || !strings.Contains(strings.ToLower(failed.Message), "input/output error") {
		t.Fatalf("NFS EIO not propagated: %+v", failed)
	}
	t.Logf("real error propagated: state=%s message=%s", failed.State, failed.Message)
	if _, err := os.Stat(filepath.Join(r.target, modelscan.CompletionMarker)); !os.IsNotExist(err) {
		t.Fatalf("failed copy was marked complete: %v", err)
	}
	elapsed := r.control(t)
	restore()
	oldTransfer := r.file.TransferID
	raw, _ := json.Marshal(map[string]uint{"storage_id": r.file.StorageID})
	response, err := r.api.PostJSON(fmt.Sprintf("/api/v1/models/%d/nodes/%s/cache/retry", r.file.ModelID, r.node.UUID), raw)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatalf("real retry API: %d %s", response.StatusCode, body)
	}
	current, err := r.env.Store().ModelFileByID(r.file.ID)
	if err != nil || current.TransferID == oldTransfer {
		t.Fatalf("retry did not rotate stopped intent: %+v %v", current, err)
	}
	// The retry itself pushes the new intent; no explicit sync here.
	r.file = current
	r.wait(t, 120*time.Second, func(st proto.ModelFileStatus) bool { return st.State == proto.MFPresent })
	r.verifyContents(t)
	t.Logf("PASS: %s -> real retry API 202 -> SHA256=%x; control+API=%s", mode, r.digest, elapsed)
}
