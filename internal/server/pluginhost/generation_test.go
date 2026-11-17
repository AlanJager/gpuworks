package pluginhost

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// Every process the host starts gets a higher generation than any before
// it, across crashes, a stop and a new Start of the same package, and other
// packages; the host API connection pushes under its own process's number.
func TestGenerationRisesWithEveryProcess(t *testing.T) {
	state := &fakeState{}
	sp := &fakeSpawner{config: func(n int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		c.Start = func(ctx context.Context, p *pluginsdk.Plugin) error {
			_, err := p.PushState(ctx, extapi.KindBanners, banners("hello"))
			if n == 1 {
				return context.Canceled // the first process ends at once: a crash
			}
			return err
		}
		return c
	}}
	h, _ := newTestHost(t, sp, state, fastTiming())
	if err := h.Start(testSpec(t, "acme_backup")); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, h, "acme_backup", func(s Status) bool { return s.State == StateRunning && s.Starts == 2 })
	if st.Generation != 2 {
		t.Fatalf("after one crash: %+v", st)
	}
	waitPushes(t, state, 2)
	pushes := state.recorded()
	if pushes[0].owner.Generation != 1 || pushes[1].owner.Generation != 2 {
		t.Fatalf("pushes came under generations %d and %d", pushes[0].owner.Generation, pushes[1].owner.Generation)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.Stop(ctx, "acme_backup"); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(testSpec(t, "acme_backup")); err != nil {
		t.Fatal(err)
	}
	st = waitStatus(t, h, "acme_backup", inState(StateRunning))
	if st.Starts != 1 || st.Generation != 3 {
		t.Fatalf("after a new Start: %+v", st)
	}
	waitPushes(t, state, 3)
	if g := state.recorded()[2].owner.Generation; g != 3 {
		t.Fatalf("the restarted plugin pushed under generation %d", g)
	}
}

func waitPushes(t *testing.T, s *fakeState, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(s.recorded()) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(s.recorded()); got < n {
		t.Fatalf("%d pushes, want %d", got, n)
	}
}
