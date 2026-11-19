package engine

import (
	"context"
	"strconv"
	"strings"
	"time"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// BuildCardSmokeWorkload is the container the per-card runtime check runs in
// on a host whose runtime lives only in images: image (a runner image already
// on the node), the one card at gpuIndex injected exactly as a real instance
// gets it, and entrypoint as its command. Nothing else of an instance's is
// added — no model, no ports, no partition.
func (e *Engine) BuildCardSmokeWorkload(image string, gpuIndex int, name string, entrypoint []string) (agentruntime.WorkloadSpec, error) {
	ws := e.workloadSpec(proto.InstanceDesiredState{UUID: name, Image: image, GPUIndexes: []int{gpuIndex}}, name)
	if err := e.applyDeviceInjection(&ws, ws.GPUIndexes); err != nil {
		return agentruntime.WorkloadSpec{}, err
	}
	ws.Entrypoint = entrypoint
	ws.Cmd = nil
	return ws, nil
}

// cardSmokeNamePrefix starts the name of every card runtime-check container.
const cardSmokeNamePrefix = "gpuworks-card-smoke-"

// CardSmokeContainerStaleAfter is how old a card runtime-check container
// must be, by the start its name records, before anyone but the agent that
// started it may take it for finished business. A check and its cleanup end
// within two minutes of that start; the rest is room for a stepped clock.
const CardSmokeContainerStaleAfter = 10 * time.Minute

// CardSmokeContainerName names the container one runtime check of the card at
// gpuIndex runs in. The start time makes it unique per attempt (Doctor and
// the periodic round may ask the same card at once) and lets anyone reading
// the runtime's inventory later tell a finished attempt from one in flight.
func CardSmokeContainerName(gpuIndex int, started time.Time) string {
	return cardSmokeNamePrefix + strconv.Itoa(gpuIndex) + "-" + strconv.FormatInt(started.UnixNano(), 36)
}

// CardSmokeContainerStarted reports whether name is a card runtime-check
// container's, and when that check started. Such a container never serves a
// deployment: it runs one short script against one card and is removed.
func CardSmokeContainerStarted(name string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(name, cardSmokeNamePrefix)
	if !ok {
		return time.Time{}, false
	}
	index, stamp, ok := strings.Cut(rest, "-")
	if !ok {
		return time.Time{}, false
	}
	if n, err := strconv.Atoi(index); err != nil || n < 0 || strconv.Itoa(n) != index {
		return time.Time{}, false
	}
	nanos, err := strconv.ParseInt(stamp, 36, 64)
	if err != nil || nanos <= 0 || strconv.FormatInt(nanos, 36) != stamp {
		return time.Time{}, false
	}
	return time.Unix(0, nanos), true
}

// DesiredImages are the images of the instances this agent is told to run.
func (e *Engine) DesiredImages() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	images := make([]string, 0, len(e.desired))
	for _, spec := range e.desired {
		if spec.Image != "" {
			images = append(images, spec.Image)
		}
	}
	return images
}

// cardGateLimit bounds how long a start waits for a runtime check on its
// cards. A check takes well under a second on a healthy card (a few on an
// Ascend die); one stuck in the driver is not waited out, the start goes on
// and the driver answers for itself.
const cardGateLimit = 10 * time.Second

// SetCardGate installs what a start waits on before it launches the container:
// the runtime check's WaitIdle. On a card only one process may open (an Ascend
// die without device-share) a check holding it would fail the engine's start.
func (e *Engine) SetCardGate(wait func(ctx context.Context, idents []string, limit time.Duration) bool) {
	e.mu.Lock()
	e.cardGate = wait
	e.mu.Unlock()
}

// waitForCardChecks waits for any runtime check on the instance's cards.
func (e *Engine) waitForCardChecks(ctx context.Context, uuid string, idents []string) {
	e.mu.Lock()
	wait := e.cardGate
	e.mu.Unlock()
	if wait == nil || len(idents) == 0 {
		return
	}
	start := time.Now()
	if !wait(ctx, idents, cardGateLimit) && ctx.Err() == nil {
		e.log.Warn("a runtime check is still on the instance's cards; starting anyway", "instance", uuid, "waited", time.Since(start).Round(time.Millisecond))
	} else if waited := time.Since(start); waited > 100*time.Millisecond {
		e.log.Info("waited for a runtime check on the instance's cards", "instance", uuid, "waited", waited.Round(time.Millisecond))
	}
}
