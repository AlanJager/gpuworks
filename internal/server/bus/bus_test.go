package bus

import (
	"testing"
	"time"
)

func TestPublishDeliver(t *testing.T) {
	b := New()
	sub := b.Subscribe(8, "node")
	defer sub.Close()

	b.Publish(Event{Topic: "node", Key: "n1", Reason: "registered"})
	select {
	case ev := <-sub.C():
		if ev.Key != "n1" {
			t.Errorf("key = %q", ev.Key)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered")
	}
}

func TestTopicFilter(t *testing.T) {
	b := New()
	sub := b.Subscribe(8, "node")
	defer sub.Close()

	b.Publish(Event{Topic: "gpu", Key: "x"}) // filtered out
	b.Publish(Event{Topic: "node", Key: "y"})
	select {
	case ev := <-sub.C():
		if ev.Key != "y" {
			t.Errorf("expected filtered stream to yield y, got %q", ev.Key)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

func TestCoalesceOnBackpressure(t *testing.T) {
	b := New()
	sub := b.Subscribe(1) // tiny buffer forces coalescing
	defer sub.Close()

	// Fill the buffer, then push many same-key events; they must coalesce.
	for i := 0; i < 50; i++ {
		b.Publish(Event{Topic: "node", Key: "same", Reason: "r"})
	}
	// Drain what we can.
	drained := 0
	timeout := time.After(200 * time.Millisecond)
loop:
	for {
		select {
		case <-sub.C():
			drained++
			sub.sub.tryFlush()
		case <-timeout:
			break loop
		}
	}
	if drained == 0 {
		t.Fatal("expected at least one delivery")
	}
	if sub.CoalescedCount() == 0 {
		t.Error("expected coalescing under backpressure")
	}
	if drained > 5 {
		t.Errorf("same-key events should coalesce, drained %d", drained)
	}
}

func TestCoalescingKeepsAStateChangeAheadOfLaterProgress(t *testing.T) {
	b := New()
	sub := b.Subscribe(1)
	defer sub.Close()

	b.Publish(Event{Topic: "instance", Key: "filler", Reason: "status"}) // occupies the buffer
	b.Publish(Event{Topic: "instance", Key: "i1", Reason: ReasonProgress})
	b.Publish(Event{Topic: "instance", Key: "i1", Reason: "status"})
	for i := 0; i < 20; i++ {
		b.Publish(Event{Topic: "instance", Key: "i1", Reason: ReasonProgress})
	}
	// A key that only ever saw progress still reports the newest progress.
	b.Publish(Event{Topic: "instance", Key: "i2", Reason: ReasonProgress})
	b.Publish(Event{Topic: "instance", Key: "i2", Reason: ReasonProgress})

	got := map[string]string{}
	for len(got) < 3 {
		select {
		case ev := <-sub.C():
			got[ev.Key] = ev.Reason
			sub.sub.tryFlush()
		case <-time.After(time.Second):
			t.Fatalf("timed out; delivered so far: %v", got)
		}
	}
	if got["i1"] != "status" {
		t.Errorf("i1 delivered with reason %q; the held state change was displaced by progress", got["i1"])
	}
	if got["i2"] != ReasonProgress {
		t.Errorf("i2 delivered with reason %q, want %q", got["i2"], ReasonProgress)
	}
}

// Generation moves with a topic's state changes and stays put for progress
// notices and other topics: a cache keyed on it rebuilds when its objects
// changed and only then.
func TestGenerationCountsATopicsStateChanges(t *testing.T) {
	b := New()
	if got := b.Generation("instance"); got != 0 {
		t.Fatalf("fresh generation = %d", got)
	}
	b.PublishChange("instance", "i1", "state:running")
	b.PublishChange("instance", "i1", ReasonProgress)
	b.PublishChange("node", "n1", "status")
	b.PublishChange("instance", "i2", "created")
	if got := b.Generation("instance"); got != 2 {
		t.Fatalf("instance generation = %d, want 2 (progress and other topics excluded)", got)
	}
	if got := b.Generation("node"); got != 1 {
		t.Fatalf("node generation = %d, want 1", got)
	}
}
