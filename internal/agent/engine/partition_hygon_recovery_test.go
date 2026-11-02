package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type unverifiedHygonHelper struct {
	*fakeHygonVdevHelper
	creates int
}

func (h *unverifiedHygonHelper) Create(_ context.Context, _, _, _ int, recordPath, _ string) (int, error) {
	h.creates++
	// The managed helper persists this before the native call. A native device
	// may now exist, but invalid vendor config prevents claiming its identity.
	if err := os.WriteFile(filepath.Join(filepath.Dir(recordPath), hygonVdevCreatePendingName), []byte("pending\n"), 0o600); err != nil {
		return 0, err
	}
	return 0, errors.New("cannot verify newly created vdev; manual inspection required")
}

func TestHygonUnverifiedCreateBlocksRetryAndCleanup(t *testing.T) {
	e, h, native := hygonTestEngine(t)
	helper := &unverifiedHygonHelper{fakeHygonVdevHelper: native}
	h.helper = helper
	spec := proto.InstanceDesiredState{UUID: "unverified-create", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	if ok, reason := h.apply(e, t.Context(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "manual inspection") {
		t.Fatalf("unverified creation accepted: %t/%s", ok, reason)
	}
	if ok, reason := h.apply(e, t.Context(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "manual recovery") {
		t.Fatalf("unverified creation retried: %t/%s", ok, reason)
	}
	if helper.creates != 1 {
		t.Fatalf("native create calls = %d, want 1", helper.creates)
	}
	if err := h.cleanupInstance(e, spec.UUID); err == nil || !strings.Contains(err.Error(), "manual recovery") {
		t.Fatalf("unclaimed device cleanup = %v", err)
	}
	if _, err := os.Stat(filepath.Join(hygonInstanceDir(e, spec.UUID), hygonVdevCreatePendingName)); err != nil {
		t.Fatalf("cleanup erased recovery evidence: %v", err)
	}
	if len(native.released) != 0 {
		t.Fatal("cleanup destroyed an unclaimed device")
	}
}

func TestHygonPendingCreateBlocksExistingOwnership(t *testing.T) {
	e, h, native := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "pending-with-ledger", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	if ok, reason := h.apply(e, t.Context(), &agentruntime.WorkloadSpec{}, spec); !ok {
		t.Fatal(reason)
	}
	marker := filepath.Join(hygonInstanceDir(e, spec.UUID), hygonVdevCreatePendingName)
	if err := os.WriteFile(marker, []byte("interrupted creation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, reason := h.apply(e, t.Context(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "manual recovery") {
		t.Fatalf("pending creation reused ledger: %t/%s", ok, reason)
	}
	if err := h.cleanupInstance(e, spec.UUID); err == nil || !strings.Contains(err.Error(), "manual recovery") {
		t.Fatalf("pending creation cleanup: %v", err)
	}
	if len(native.released) != 0 || len(native.createdCUs) != 1 {
		t.Fatalf("pending resource modified: creates=%v, releases=%v", native.createdCUs, native.released)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cleanup erased pending creation: %v", err)
	}
}

func TestHygonCleanupWithoutOwnership(t *testing.T) {
	for _, state := range []string{"absent", "empty", "snapshot", "unreadable-directory"} {
		t.Run(state, func(t *testing.T) {
			e, h, native := hygonTestEngine(t)
			dir := hygonInstanceDir(e, state)
			if state != "absent" {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if state == "snapshot" {
				if err := os.WriteFile(filepath.Join(dir, "vdev4.conf"), []byte("partial state"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if state == "unreadable-directory" {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory permissions")
				}
				if err := os.Chmod(dir, 0o111); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			}
			err := h.cleanupInstance(e, state)
			wantError := state == "snapshot" || state == "unreadable-directory"
			if (err != nil) != wantError {
				t.Fatalf("cleanup %s: %v, want error %t", state, err, wantError)
			}
			if len(native.released) != 0 {
				t.Fatal("cleanup released an unowned device")
			}
		})
	}
}
