package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

// stopProbeDriver forces every container to exist and running so
// removeContainer walks its stop path; the optional stopErr reproduces a stop
// that fails (for example a graceful-window timeout).
type stopProbeDriver struct {
	*recording.Driver
	stopErr error
}

func (d *stopProbeDriver) Status(context.Context, string) (runtime.Status, error) {
	return runtime.Status{Exists: true, Running: true}, nil
}

func (d *stopProbeDriver) Stop(ctx context.Context, id string) error {
	if d.stopErr != nil {
		return d.stopErr
	}
	return d.Driver.Stop(ctx, id)
}

// The stop inside removeContainer used to be silent and discarded its error,
// which is how an externally killed container looked like it had never existed.
// It must log at INFO on the way in and surface a stop failure as a warning
// while the forced removal still proceeds.
func TestRemoveContainerLogsStopAndSurfacesStopFailure(t *testing.T) {
	newEngine := func(driver runtime.Driver, logs *bytes.Buffer) *Engine {
		return New(driver, func(proto.InstanceStatus) {}, slog.New(slog.NewTextHandler(logs, nil)), t.TempDir())
	}
	ctx := context.Background()

	var logs bytes.Buffer
	e := newEngine(&stopProbeDriver{Driver: recording.New(t.TempDir() + "/calls.json")}, &logs)
	if err := e.removeContainer(ctx, "instance-stop-ok"); err != nil {
		t.Fatal(err)
	}
	if got := logs.String(); !strings.Contains(got, "stopping instance container") || !strings.Contains(got, "gpuworks-instance-s") {
		t.Fatalf("expected INFO stop log with container name, got: %q", got)
	}
	if strings.Contains(logs.String(), "stop failed") {
		t.Fatalf("successful stop must not warn: %q", logs.String())
	}

	logs.Reset()
	e = newEngine(&stopProbeDriver{Driver: recording.New(t.TempDir() + "/calls.json"), stopErr: errors.New("stop timeout")}, &logs)
	if err := e.removeContainer(ctx, "instance-stop-fail"); err != nil {
		t.Fatalf("a failed stop must not fail the removal while rm still succeeds: %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "instance container stop failed") || !strings.Contains(got, "stop timeout") {
		t.Fatalf("expected stop-failure warning, got: %q", got)
	}
}
