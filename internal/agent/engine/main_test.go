package engine

import (
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// TestMain turns the pre-start port check off for the package's tests: their
// stand-in engines listen on the API port before the fake runtime "starts"
// the container, which a real engine never does. TestStartRefusesAPortTaken*
// puts the real check back on its own engine.
func TestMain(m *testing.M) {
	defaultPortCheck = func(proto.InstanceDesiredState) error { return nil }
	os.Exit(m.Run())
}
