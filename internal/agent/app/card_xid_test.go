package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

func xidNode(t *testing.T, log string) (*NodeRuntime, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kmsg")
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	boot := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	w := &gpu.XidWatcher{Path: path, BootTime: func() time.Time { return boot }}
	w.Run(context.Background())
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none", nil, nil)
	n.xid.Store(w)
	return n, boot
}

func xidCards() []proto.GPUInfo {
	return []proto.GPUInfo{
		{Index: 0, UUID: "GPU-17", Vendor: "nvidia", PCIeBDF: "00000000:17:00.0", Healthy: true, HealthStatus: accelerator.HealthOK},
		{Index: 1, UUID: "GPU-3b", Vendor: "nvidia", PCIeBDF: "00000000:3B:00.0", Healthy: true, HealthStatus: accelerator.HealthOK},
	}
}

// An Xid that says the card is broken holds it until a runtime check has
// passed on it since; the application's own Xid changes nothing.
func TestXidHoldsACardUntilARuntimeCheckPassesAfterIt(t *testing.T) {
	n, boot := xidNode(t, "4,1,7000000,-;NVRM: Xid (PCI:0000:17:00): 13, pid=42, name=python3, Graphics SM Warp Exception\n"+
		"4,2,9000000,-;NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.\n"+
		"4,3,9500000,-;NVRM: Xid (PCI:0000:3b:00): 119, pid=1, name=nvidia-smi, Timeout after 6s of waiting for RPC response\n")
	event := boot.Add(9500 * time.Millisecond)
	var passedAt time.Time
	passed := func(ident string, t time.Time) bool {
		return ident == accelerator.CardIdent("GPU-3b", "00000000:3B:00.0") && passedAt.After(t)
	}

	cards := xidCards()
	n.applyXidFindings(cards, passed)
	if cards[0].HealthCode != "" {
		t.Fatalf("an application Xid changed the card: %+v", cards[0])
	}
	if c := cards[1]; c.HealthCode != accelerator.HealthRebootRequired || c.Healthy ||
		c.HealthMessage != "Xid 79=pid='<unknown>', name=<unknown>, GPU has fallen off the bus. (2026-11-01 08:00:09Z)\n"+
			"Xid 119=pid=1, name=nvidia-smi, Timeout after 6s of waiting for RPC response (2026-11-01 08:00:09Z)" {
		t.Fatalf("card after Xid 79 and 119 = %+v", c)
	}

	// A check that passed between the two events clears only the earlier.
	passedAt = boot.Add(9200 * time.Millisecond)
	cards = xidCards()
	n.applyXidFindings(cards, passed)
	if cards[1].HealthCode != accelerator.HealthResetRequired {
		t.Fatalf("after a check between the events = %q, want the later Xid 119 still holding", cards[1].HealthCode)
	}
	passedAt = event.Add(time.Minute)
	cards = xidCards()
	n.applyXidFindings(cards, passed)
	if cards[1].HealthCode != "" || !cards[1].Healthy {
		t.Fatalf("after a check passed since = %+v", cards[1])
	}
}

// A node without the watcher (not NVIDIA, or no log) changes nothing.
func TestNoXidWatcherChangesNothing(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none", nil, nil)
	cards := xidCards()
	n.applyXidFindings(cards, nil)
	if cards[1].HealthCode != "" {
		t.Fatal("changed")
	}
}
