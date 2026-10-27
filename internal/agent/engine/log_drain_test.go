package engine

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// drainCall is what one LogDrain call saw of the container it was handed.
type drainCall struct {
	uuid     string
	exited   bool
	exists   bool // the container still existed while the drain ran
	running  bool
	released bool
	// existsAtRelease: the container still existed when release ran.
	existsAtRelease bool
}

type drainRecorder struct {
	mu    sync.Mutex
	calls []*drainCall
}

func (r *drainRecorder) drain(driver *livenessDriver) LogDrain {
	return func(ctx context.Context, uuid string, exited bool) func() {
		st, _ := driver.Status(ctx, containerName(uuid))
		call := &drainCall{uuid: uuid, exited: exited, exists: st.Exists, running: st.Running}
		r.mu.Lock()
		r.calls = append(r.calls, call)
		r.mu.Unlock()
		return func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			call.released = true
			call.existsAtRelease = driver.exists(containerName(uuid))
		}
	}
}

func (r *drainRecorder) snapshot() []drainCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]drainCall, len(r.calls))
	for i, call := range r.calls {
		out[i] = *call
	}
	return out
}

// A container that dies on its own is drained while its output still exists:
// after it stopped, before it is removed, and told that it exited by itself.
func TestLogDrainRunsBetweenAContainerExitAndItsRemoval(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	rec := &drainRecorder{}
	WithLogDrain(rec.drain(driver))(e)
	spec := runningSpec("drain-crash", healthyEngine(t))
	serve(t, e, log, spec)

	driver.exit(containerName(spec.UUID), 127, false)
	e.checkLiveness(context.Background())
	log.waitFor(t, proto.InstFailed, 1)

	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("drain calls = %+v, want one", calls)
	}
	got := calls[0]
	if got.uuid != spec.UUID || !got.exited || !got.exists || got.running {
		t.Fatalf("drain saw %+v, want the exited container of %s before its removal", got, spec.UUID)
	}
	if !got.released || got.existsAtRelease {
		t.Fatalf("drain release = %+v, want it to run once the container is gone", got)
	}
	waitIdle(t, e, spec.UUID)
}

// A removal that stops a running container drains after the stop, so the
// engine's shutdown lines are included, and says the container did not exit
// by itself.
func TestLogDrainAfterStoppingARunningContainer(t *testing.T) {
	driver := newLivenessDriver()
	e, _ := newLivenessEngine(t, driver)
	rec := &drainRecorder{}
	WithLogDrain(rec.drain(driver))(e)
	driver.containers[containerName("drain-stop")] = &livenessContainer{running: true}

	if err := e.removeContainer(context.Background(), "drain-stop"); err != nil {
		t.Fatal(err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("drain calls = %+v, want one", calls)
	}
	if got := calls[0]; got.exited || !got.exists || got.running || !got.released || got.existsAtRelease {
		t.Fatalf("drain saw %+v, want a stopped (not exited) container drained before removal", got)
	}
}

// release runs even when the removal fails, and nothing is drained for a
// container that does not exist.
func TestLogDrainReleasesAfterAFailedRemovalAndSkipsAbsentContainers(t *testing.T) {
	driver := newLivenessDriver()
	e, _ := newLivenessEngine(t, driver)
	rec := &drainRecorder{}
	WithLogDrain(rec.drain(driver))(e)
	driver.containers[containerName("drain-rm-fails")] = &livenessContainer{exitCode: 1}
	driver.removeErrs = 1

	err := e.removeContainer(context.Background(), "drain-rm-fails")
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("removeContainer err = %v, want the injected removal failure", err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 || !calls[0].exited || !calls[0].released || !calls[0].existsAtRelease {
		t.Fatalf("drain calls = %+v, want one released drain with the container still there", calls)
	}

	if err := e.removeContainer(context.Background(), "drain-absent"); err != nil {
		t.Fatal(err)
	}
	if calls := rec.snapshot(); len(calls) != 1 {
		t.Fatalf("an absent container was drained: %+v", calls)
	}
}

// An engine without a drain removes containers as before.
func TestRemoveContainerWithoutLogDrain(t *testing.T) {
	driver := newLivenessDriver()
	e, _ := newLivenessEngine(t, driver)
	driver.containers[containerName("no-drain")] = &livenessContainer{exitCode: 1}
	if err := e.removeContainer(context.Background(), "no-drain"); err != nil {
		t.Fatal(err)
	}
	if driver.exists(containerName("no-drain")) {
		t.Fatal("the container was not removed")
	}
}

// Even a CLI/model inspection failure before the first health check must have
// its full diagnostic drained before runtime removal destroys the output.
func TestStartupFailureDrainsDiagnosticBeforeRemoval(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("startup-pooling-failure", 1)
	name := containerName(spec.UUID)
	driver.containers[name] = &livenessContainer{exitCode: 1}
	driver.logs = "Traceback: model registry inspection\nValueError: Model architectures failed to be inspected\n"
	var drained string
	released := false
	WithLogDrain(func(ctx context.Context, uuid string, exited bool) func() {
		if uuid != spec.UUID || !exited || !driver.exists(name) {
			t.Errorf("drain did not see the exited startup container")
		}
		output, err := driver.Logs(ctx, name, false, 200)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(output)
		output.Close()
		if err != nil {
			t.Fatal(err)
		}
		drained = string(data)
		return func() { released = true }
	})(e)
	if e.probeAndReport(t.Context(), spec, name) {
		t.Fatal("failed startup marked ready")
	}
	if drained != driver.logs || !released || driver.exists(name) {
		t.Fatalf("startup diagnostic not retained before removal: %q released=%v", drained, released)
	}
	states := log.states(proto.InstFailed)
	if len(states) != 1 || !strings.Contains(states[0].Message, "failed to be inspected") {
		t.Fatalf("failed startup status: %+v", states)
	}
}
