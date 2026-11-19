package engine

import (
	"strings"
	"testing"
	"time"
)

func TestCardSmokeContainerNamesCarryTheirStartTime(t *testing.T) {
	at := time.Unix(1794787200, 123456789)
	name := CardSmokeContainerName(7, at)
	got, ok := CardSmokeContainerStarted(name)
	if !ok || !got.Equal(at) || !strings.HasPrefix(name, "gpuworks-card-smoke-7-") {
		t.Fatalf("%s -> %v, %v", name, got, ok)
	}
	// The name the field reported.
	if _, ok := CardSmokeContainerStarted("gpuworks-card-smoke-1-dmtplbn8ojg7"); !ok {
		t.Fatal("a real check container name was not recognised")
	}
	for _, other := range []string{"gpuworks-59e3b54200d29e6e6bcce0911b550040", "gpuworks-card-smoke-", "gpuworks-card-smoke-1", "gpuworks-card-smoke-1-",
		"gpuworks-card-smoke--1-abc", "gpuworks-card-smoke-01-abc", "gpuworks-card-smoke-1-ABC", "gpuworks-card-smoke-1-abc-def", "card-smoke-1-abc", "gpuworks-card-smoke-1-0"} {
		if _, ok := CardSmokeContainerStarted(other); ok {
			t.Errorf("%q was taken for a check container", other)
		}
	}
}
