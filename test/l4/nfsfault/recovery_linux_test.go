//go:build linux && l4

package nfsfault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestL4StorageModelCacheRecoveryNFS is l4-storage-model-cache-recovery-nfs.
// It uses the real server, agent, WebSocket, SQLite, kernel NFS client and
// Ganesha. Only the unused GPU/container runtime are supplied by testenv.
// The caller MUST isolate mounts: unshare --mount --propagation private.
func TestL4StorageModelCacheRecoveryNFS(t *testing.T) {
	lab := newNFSLab(t)
	for _, mode := range []string{"blackhole", "pause", "crash", "loss", "cancel", "disk_full", "eio"} {
		if !t.Run(mode, func(t *testing.T) {
			if mode == "disk_full" || mode == "eio" {
				runFailureRetry(t, lab, mode)
			} else {
				runRecovery(t, lab, mode)
			}
		}) {
			break
		}
	}
}

type copyRun struct {
	env            *testenv.Env
	api            *testenv.APIClient
	file           *store.ModelFile
	node           *store.Node
	source, target string
	digest         [32]byte
}

func newCopyRun(t *testing.T, lab *nfsLab, mode string) *copyRun {
	t.Helper()
	rel := mode
	source := filepath.Join(lab.export, rel)
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	// Distinct inodes for every scenario; never read the NFS weights before
	// copying. No global drop_caches or shared production files are needed.
	config := []byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`)
	if err := os.WriteFile(filepath.Join(source, "config.json"), config, 0644); err != nil {
		t.Fatal(err)
	}
	weights := bytes.Repeat([]byte("nfs-fault-content"), 4<<20) // 68 MiB
	digest := sha256.Sum256(weights)
	if err := os.WriteFile(filepath.Join(source, "model.safetensors"), weights, 0644); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	if mode == "disk_full" {
		if err := os.Mkdir(cache, 0755); err != nil {
			t.Fatal(err)
		}
		// The size is a quota, not a 2 GiB allocation. Only the copied bytes use RAM.
		mustCommand(t, "mount", "-t", "tmpfs", "-o", "size=2G,nosuid,nodev", "tmpfs", cache)
		t.Cleanup(func() { mustCommand(t, "umount", cache) })
	}
	env := testenv.New(t)
	env.AddNode("nfs-client", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}))
	env.WaitNodeOnline(t, "nfs-client")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes: %v %v", nodes, err)
	}
	node := &nodes[0]
	api := env.API(t)
	api.Client.Timeout = 5 * time.Second
	raw, _ := json.Marshal(map[string]any{"name": "fault-cache", "type": "nfs_cache", "path": lab.mount, "export": lab.address + ":/models", "existing": true, "cache_path": cache, "enabled": true, "all_nodes": true})
	resp, err := api.PostJSON("/api/v1/storages", raw)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create storage: %d %s", resp.StatusCode, data)
	}
	var storage struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(data, &storage); err != nil || storage.ID == 0 {
		t.Fatalf("storage: %s %v", data, err)
	}
	env.Eventually(t, 30*time.Second, func() bool {
		state, err := env.Store().StorageNodeStateFor(storage.ID, node.ID)
		return err == nil && state.State == proto.StorageStateReady
	})
	model, err := env.Store().UpsertModel(&store.Model{Name: "fault-model", Source: "local", SourceID: mode, WeightBytes: int64(len(weights))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().RegisterModelFile(store.FileRef{ModelID: model.ID, StorageID: storage.ID}, rel, int64(len(weights)+len(config))); err != nil {
		t.Fatal(err)
	}
	file, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storage.ID, NodeID: node.ID, Path: rel,
		State: proto.MFPending, Acquisition: proto.ModelAcquireCopy, TransferID: "fault-" + mode, RateLimit: 2048, SizeBytes: int64(len(weights) + len(config))})
	if err != nil {
		t.Fatal(err)
	}
	// The fixture seeds desired state directly; sync through the real hub/WS.
	if err := env.Hub().SyncNode(node.UUID); err != nil {
		t.Fatal(err)
	}
	return &copyRun{env: env, api: api, file: file, node: node, source: source, target: filepath.Join(cache, rel), digest: digest}
}

func (r *copyRun) observe(t *testing.T) proto.ModelFileStatus {
	t.Helper()
	file, err := r.env.Store().ModelFileByID(r.file.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := r.env.Hub().ModelCopyObservation(file); ok {
		return st
	}
	return proto.ModelFileStatus{State: file.State, Message: file.Message, BytesDone: file.Downloaded, BytesTotal: file.SizeBytes, TransferID: file.TransferID}
}

func (r *copyRun) control(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := r.env.Hub().CallNode(ctx, r.node.UUID, proto.RPCFSStat, proto.FSStatParams{Path: r.env.DataDir()})
	var stat proto.FSStatResult
	if err != nil || json.Unmarshal(raw, &stat) != nil || !stat.Dir {
		t.Fatalf("independent control RPC failed: %s %v", raw, err)
	}
	response, err := r.api.Get(fmt.Sprintf("/api/v1/models/%d", r.file.ModelID))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("management API=%d", response.StatusCode)
	}
	return time.Since(start)
}

func (r *copyRun) wait(t *testing.T, timeout time.Duration, condition func(proto.ModelFileStatus) bool) proto.ModelFileStatus {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		last := r.observe(t)
		if last.State == proto.MFFailed || last.State == proto.MFInsufficientDisk {
			t.Fatalf("unexpected terminal copy: %+v", last)
		}
		if condition(last) {
			return last
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("copy condition timeout after %s: %+v preparation=%+v", timeout, last, last.Preparation)
		}
	}
}

func runRecovery(t *testing.T, lab *nfsLab, mode string) {
	r := newCopyRun(t, lab, mode)
	partial := r.wait(t, 30*time.Second, func(st proto.ModelFileStatus) bool {
		return st.Preparation != nil && st.Preparation.Phase == proto.ModelPhaseCopying && st.BytesDone > 1<<20 && st.BytesDone < st.BytesTotal/2
	})
	t.Logf("observed real partial copy=%d/%d before fault", partial.BytesDone, partial.BytesTotal)
	restore := lab.fault(t, mode)
	defer restore() // Runs before Env cleanup, including assertion failures.
	var maxControl time.Duration
	var lastCheck time.Time
	faultStarted := time.Now()
	checks := 0
	check := func() {
		if time.Since(lastCheck) < time.Second {
			return
		}
		lastCheck = time.Now()
		elapsed := r.control(t)
		if elapsed > maxControl {
			maxControl = elapsed
		}
		checks++
	}
	if mode == "loss" {
		// Recovery must make real forward progress despite loss, not just return a
		// successful tc command. The final hash checks every byte afterwards.
		r.wait(t, 90*time.Second, func(st proto.ModelFileStatus) bool { check(); return st.BytesDone > partial.BytesDone+4<<20 })
	} else {
		idle := int64(5)
		timeout := 45 * time.Second
		if mode == "blackhole" {
			idle = 120
			timeout = 170 * time.Second
		}
		stalled := r.wait(t, timeout, func(st proto.ModelFileStatus) bool {
			check()
			return st.Preparation != nil && int64(st.Preparation.IdleSeconds) >= idle
		})
		if stalled.BytesDone >= stalled.BytesTotal || stalled.State != proto.MFDownloading {
			t.Fatalf("fault failed to block: %+v", stalled)
		}
		if mode == "blackhole" && (!stalled.Preparation.Stalled || stalled.RateBytesPerS != 0) {
			t.Fatalf("missing stalled/zero rate: %+v", stalled)
		}
		t.Logf("confirmed blocked read: bytes=%d/%d idle=%ds stalled=%v", stalled.BytesDone, stalled.BytesTotal, stalled.Preparation.IdleSeconds, stalled.Preparation.Stalled)
		if mode == "cancel" {
			r.cancelBlocked(t)
			check()
		}
	}
	if mode == "loss" || (mode == "blackhole" && os.Getenv("GPUWORKS_NFS_FAULT_NETWORK") != "link") {
		stats := mustCommand(t, "docker", "exec", lab.name, "tc", "-s", "qdisc", "show", "dev", "eth0")
		if !regexp.MustCompile(`dropped [1-9][0-9]*`).MatchString(stats) {
			t.Fatalf("fault did not drop real packets: %s", stats)
		}
		t.Logf("actual netem counters:\n%s", stats)
	}
	if mode == "blackhole" {
		node, err := r.env.Store().NodeByUUID(r.node.UUID)
		if err != nil || node.LastSeenAt == nil || !node.LastSeenAt.After(faultStarted) {
			t.Fatalf("heartbeat stopped during NFS fault: %+v %v", node, err)
		}
		t.Logf("heartbeat advanced during blackhole by %s", node.LastSeenAt.Sub(faultStarted))
	}
	restore()
	if mode == "cancel" {
		r.resumeCanceled(t)
	}
	r.wait(t, 120*time.Second, func(st proto.ModelFileStatus) bool { return st.State == proto.MFPresent })
	r.verifyContents(t)
	t.Logf("PASS: restored SHA256=%x; control+API checks=%d worst=%s", r.digest, checks, maxControl)
}

func (r *copyRun) cancelBlocked(t *testing.T) {
	t.Helper()
	// Durable withdrawal is the same boundary used by last-owner cancellation.
	if err := r.env.Store().SetModelFileState(r.file.ID, proto.MFAbsent, "fault test withdrawal"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := r.env.Hub().CallNode(ctx, r.node.UUID, proto.RPCModelCopyStop, proto.ModelCopyStopParams{Key: store.ModelFileKey(r.file.ID), TransferID: r.file.TransferID})
	if err == nil {
		t.Fatal("hard-mounted blocked worker falsely confirmed exit")
	}
	if _, err := os.Stat(filepath.Join(r.target, modelscan.CompletionMarker)); !os.IsNotExist(err) {
		t.Fatalf("canceled worker installed marker: %v", err)
	}
	current, _ := r.env.Store().ModelFileByID(r.file.ID)
	if current.TransferID != r.file.TransferID {
		t.Fatal("cancellation replaced a live writer")
	}
	t.Logf("stop deadline respected; old intent retained until blocked worker returns")
}

func (r *copyRun) resumeCanceled(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	raw, err := r.env.Hub().CallNode(ctx, r.node.UUID, proto.RPCModelCopyStop, proto.ModelCopyStopParams{Key: store.ModelFileKey(r.file.ID), TransferID: r.file.TransferID})
	var result proto.ModelCopyStopResult
	if err != nil || json.Unmarshal(raw, &result) != nil || !result.Stopped {
		t.Fatalf("old worker did not exit after recovery: %s %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(r.target, "model.safetensors")); !os.IsNotExist(err) {
		t.Fatalf("canceled worker late-installed weights: %v", err)
	}
	file, err := r.env.Store().PrepareModelCopy(store.FileRef{ModelID: r.file.ModelID, StorageID: r.file.StorageID, NodeID: r.node.ID}, r.target)
	if err != nil || file.TransferID == r.file.TransferID {
		t.Fatalf("new intent: %+v %v", file, err)
	}
	t.Logf("old worker exit confirmed; new intent=%s", file.TransferID)
	r.file = file
	if err := r.env.Hub().SyncNode(r.node.UUID); err != nil {
		t.Fatal(err)
	}
}

func (r *copyRun) verifyContents(t *testing.T) {
	t.Helper()
	file, err := os.Open(filepath.Join(r.target, "model.safetensors"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	_ = file.Close()
	if err != nil || !bytes.Equal(hash.Sum(nil), r.digest[:]) {
		t.Fatalf("restored content mismatch: %v", err)
	}
}
