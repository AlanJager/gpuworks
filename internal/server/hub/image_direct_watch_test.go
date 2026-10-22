package hub

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A node-side transfer only moves while the agent reports. When those reports
// stop — agent restarted, network gone, runtime hung — the row has to become
// retryable instead of staying "in progress" with a frozen progress bar.
func TestWatchDirectImageTransferAbandonsASilentTransfer(t *testing.T) {
	st, err := store.Open("file:hub_direct_watch_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&store.Image{ImageRef: "quay.io/team/runner:cann8.5", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.CreateNodePending("gpu3")
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.UpsertImageNode(&store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID,
		State: store.ImageStateSyncing, Source: "url", Message: "downloading image archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The transfer stopped reporting ten minutes ago; nothing else touched it
	// since, which is what silence looks like in the store.
	if err := st.DB().Model(&store.ImageNode{}).Where("id = ?", row.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	previousCheck, previousAfter := imageDirectStallCheck, imageDirectStallAfter
	imageDirectStallCheck, imageDirectStallAfter = 5*time.Millisecond, time.Second
	t.Cleanup(func() {
		imageDirectStallCheck, imageDirectStallAfter = previousCheck, previousAfter
	})

	h := New(st, slog.Default())
	h.watchDirectImageTransfer(image.ID, node.ID)

	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := st.ImageNode(image.ID, node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == store.ImageStateFailed {
			if !strings.Contains(current.Message, "stopped reporting progress") {
				t.Fatalf("failure message = %q", current.Message)
			}
			if current.TransferID != "" || current.RateBPS != 0 {
				t.Fatalf("an abandoned row keeps stale transfer bookkeeping: %+v", current)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the row stayed %q after the watchdog ran", current.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A transfer that keeps reporting is left alone: the watchdog must never fail
// a download that is simply slow.
func TestWatchDirectImageTransferLeavesAReportingTransferAlone(t *testing.T) {
	st, err := store.Open("file:hub_direct_watch_live_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&store.Image{ImageRef: "quay.io/team/runner:live", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.CreateNodePending("gpu3-live")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID,
		State: store.ImageStateSyncing, Source: "url", Message: "downloading image archive",
	}); err != nil {
		t.Fatal(err)
	}

	previousCheck, previousAfter := imageDirectStallCheck, imageDirectStallAfter
	imageDirectStallCheck, imageDirectStallAfter = 5*time.Millisecond, time.Minute
	t.Cleanup(func() {
		imageDirectStallCheck, imageDirectStallAfter = previousCheck, previousAfter
	})

	h := New(st, slog.Default())
	h.watchDirectImageTransfer(image.ID, node.ID)
	time.Sleep(100 * time.Millisecond)

	current, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != store.ImageStateSyncing {
		t.Fatalf("a reporting transfer was failed: %+v", current)
	}
}

// A registry pull prints one line per state change, so a large layer can be
// silent for many minutes while it downloads perfectly well. The watchdog must
// not call that dead; a retry is still allowed, because a pull that has been
// quiet that long is not something to keep other work waiting for.
func TestWatchDirectImageTransferLeavesAQuietRegistryPull(t *testing.T) {
	st, err := store.Open("file:hub_direct_watch_pull_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	image, err := st.UpsertImage(&store.Image{ImageRef: "quay.io/team/runner:quiet", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := st.CreateNodePending("gpu3-quiet")
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.UpsertImageNode(&store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: node.ID,
		State: store.ImageStatePulling, Source: "quay", Message: "5.5GB: Downloading",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.ImageNode{}).Where("id = ?", row.ID).
		Update("updated_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	previousCheck, previousAfter := imageDirectStallCheck, imageDirectStallAfter
	imageDirectStallCheck, imageDirectStallAfter = 5*time.Millisecond, time.Second
	t.Cleanup(func() {
		imageDirectStallCheck, imageDirectStallAfter = previousCheck, previousAfter
	})

	h := New(st, slog.Default())
	h.watchDirectImageTransfer(image.ID, node.ID)
	time.Sleep(100 * time.Millisecond)

	current, err := st.ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != store.ImageStatePulling {
		t.Fatalf("a quiet registry pull was called dead: %+v", current)
	}
}
