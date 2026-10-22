package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// orphanSweepDriver records stop/remove so the sweep's footprint is observable
// without a real container runtime.
type orphanSweepDriver struct {
	*recording.Driver
	listed  []string
	stopped []string
	removed []string
}

func (d *orphanSweepDriver) ListContainers(context.Context) ([]string, error) {
	return append([]string(nil), d.listed...), nil
}

func (d *orphanSweepDriver) Stop(_ context.Context, id string) error {
	d.stopped = append(d.stopped, id)
	return nil
}

func (d *orphanSweepDriver) Remove(_ context.Context, id string) error {
	d.removed = append(d.removed, id)
	return nil
}

var _ agentruntime.Driver = (*orphanSweepDriver)(nil)

// The orphan sweep must stay off unless the server's orphan_cleanup setting
// explicitly enables it: a default-on sweep deletes this control plane's
// containers whenever a second gpuworks agent shares the runtime.
func TestOnStateSyncGatesOrphanCleanup(t *testing.T) {
	driver := &orphanSweepDriver{
		Driver: recording.New(filepath.Join(t.TempDir(), "calls.json")),
		listed: []string{"gpuworks-111111112222"},
	}
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), driver, t.TempDir(), "", metricsOnlyProber{}, nil)

	n.OnStateSync(context.Background(), proto.DesiredState{})
	if len(driver.stopped) != 0 || len(driver.removed) != 0 {
		t.Fatalf("sweep ran although orphan cleanup is disabled: stopped=%v removed=%v", driver.stopped, driver.removed)
	}

	n.OnStateSync(context.Background(), proto.DesiredState{OrphanCleanup: true})
	if len(driver.stopped) != 1 || driver.stopped[0] != "gpuworks-111111112222" {
		t.Fatalf("enabled sweep did not stop the orphan: %v", driver.stopped)
	}
	if len(driver.removed) != 1 || driver.removed[0] != "gpuworks-111111112222" {
		t.Fatalf("enabled sweep did not remove the orphan: %v", driver.removed)
	}
}
