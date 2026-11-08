package planning

import (
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func attempt(uuid string, at time.Time, message string) store.Instance {
	return store.Instance{UUID: uuid, State: proto.InstFailed, Message: message, CreatedAt: at}
}

// The budget skips the oldest FreePortRefusals refusals, whatever order the
// rows come in, and charges every other failure: a later refusal, a failure
// that started something, and the old ENGINE_PORT_IN_USE text.
func TestChargedAttemptsSkipTheFirstPortRefusals(t *testing.T) {
	base := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
	refused := func(i int) store.Instance {
		return attempt(fmt.Sprintf("r%d", i), base.Add(time.Duration(i)*time.Second), fmt.Sprintf("%s: port %d is already in use on this node; nothing was started", proto.EnginePortRefused, 30000+i))
	}
	var attempts []store.Instance
	for i := FreePortRefusals + 1; i >= 0; i-- { // newest first: order must not matter
		attempts = append(attempts, refused(i))
	}
	attempts = append(attempts,
		attempt("pull", base.Add(-time.Minute), "image pull timed out"),
		attempt("legacy", base.Add(-2*time.Minute), proto.EnginePortInUse+": port 30000 is already in use on this node"),
	)
	charged := map[string]bool{}
	for _, inst := range ChargedAttempts(attempts) {
		charged[inst.UUID] = true
	}
	for i := 0; i <= FreePortRefusals+1; i++ {
		want := i >= FreePortRefusals
		if got := charged[fmt.Sprintf("r%d", i)]; got != want {
			t.Errorf("refusal %d charged = %v, want %v", i, got, want)
		}
	}
	if !charged["pull"] || !charged["legacy"] {
		t.Errorf("charged = %v, want the pull failure and the legacy text charged", charged)
	}
	if RetryAtOnce(attempts) {
		t.Error("RetryAtOnce after a charged refusal, want the backoff")
	}
	if !RetryAtOnce([]store.Instance{attempts[len(attempts)-1], refused(0)}) {
		t.Error("RetryAtOnce = false after a free refusal")
	}
	if RetryAtOnce(nil) || RetryAtOnce([]store.Instance{attempt("x", base, "engine exited")}) {
		t.Error("RetryAtOnce without a refusal")
	}
}
