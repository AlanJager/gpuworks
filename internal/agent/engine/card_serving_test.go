package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The cards other instances serve on are what tells a start that failed on a
// shared card apart from a card nothing can use. Three instances are placed
// on one card: two serve, the third dies at startup. Neither the one asked
// about nor the one that never served counts, and an instance whose container
// has exited stops counting as soon as the monitor has seen it.
func TestServingCardsBesidesCountsOnlyTheOtherInstancesThatServe(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	e.prober = &gpu.FixedProber{VendorName: "nvidia", GPUs: []proto.GPUInfo{nvidiaCard(0, "0000:c1:00.0"), nvidiaCard(3, "0000:c4:00.0")}}
	const die = "pci:0000:c1:00.0"
	onDie := func(uuid string, port int) proto.InstanceDesiredState {
		spec := runningSpec(uuid, port)
		spec.GPUIndexes, spec.GPUIdents = []int{0}, []string{die}
		return spec
	}
	first, second := onDie("slice-a", healthyEngine(t)), onDie("slice-b", healthyEngine(t))
	// Nothing listens on the third's health port: it is still starting.
	third := onDie("slice-c", 1)
	// Bound by index only, as a server that sends no identity binds it.
	elsewhere := runningSpec("whole-card", healthyEngine(t))
	elsewhere.GPUIndexes = []int{3}

	if got := e.ServingCardsBesides("slice-c"); len(got.Idents)+len(got.Indexes) != 0 {
		t.Fatalf("serving before anything started = %+v", got)
	}
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{first, second, third, elsewhere})
	log.waitFor(t, proto.InstRunning, 3)
	for _, spec := range []proto.InstanceDesiredState{first, second, elsewhere} {
		waitWatched(t, e, spec.UUID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !driver.exists(containerName(third.UUID)) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	driver.exit(containerName(third.UUID), 1, false)
	log.waitFor(t, proto.InstFailed, 1)
	waitIdle(t, e, third.UUID)

	got := e.ServingCardsBesides(third.UUID)
	if !slices.Equal(got.Idents, []string{die, die}) || !slices.Equal(got.Indexes, []int{3}) {
		t.Fatalf("serving besides the failed start = %+v, want the two slices' die and the index-bound card", got)
	}
	if got := e.ServingCardsBesides(first.UUID); !slices.Equal(got.Idents, []string{die}) {
		t.Fatalf("serving besides a serving slice = %+v, want only the other slice (the failed start never served)", got)
	}

	// Both slices die. Until the monitor has looked they still count; once it
	// has retired them the die has nobody serving on it.
	driver.exit(containerName(first.UUID), 1, false)
	driver.exit(containerName(second.UUID), 1, false)
	e.checkLiveness(context.Background())
	log.waitFor(t, proto.InstFailed, 3)
	if got := e.ServingCardsBesides(third.UUID); len(got.Idents) != 0 || !slices.Equal(got.Indexes, []int{3}) {
		t.Fatalf("serving after the slices exited = %+v, want the die left out", got)
	}
}
