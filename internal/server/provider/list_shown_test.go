package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A page waits for the first read of the cloud; after that it is answered the
// last listing at once, and a slow read behind it replaces it when it lands.
func TestShownListingIsTheLastOneAtOnceWhileTheCloudIsReadAgain(t *testing.T) {
	var cache listCache[[]string]
	first := &gatedFetch{value: []string{"c-1"}}
	v, at, refreshing, err := cache.shown(context.Background(), 1, time.Hour, 10*time.Millisecond, first.fetch)
	if err != nil || len(v) != 1 || refreshing || at.IsZero() {
		t.Fatalf("first read = %v %v %v %v; want the cloud's listing", v, at, refreshing, err)
	}
	// Younger than refreshAfter: the cloud is not asked.
	if _, _, _, err := cache.shown(context.Background(), 1, time.Hour, 10*time.Millisecond, first.fetch); err != nil || first.asked() != 1 {
		t.Fatalf("a fresh listing asked the cloud again: %d reads, %v", first.asked(), err)
	}
	// Older than refreshAfter, and the cloud is slow: the old listing comes
	// back within the wait, marked as being refreshed; one read is made.
	slow := &gatedFetch{gate: make(chan struct{}), value: []string{"c-1", "c-2"}}
	start := time.Now()
	v, oldAt, refreshing, err := cache.shown(context.Background(), 1, 0, 20*time.Millisecond, slow.fetch)
	if err != nil || len(v) != 1 || !refreshing || !oldAt.Equal(at) || time.Since(start) > 2*time.Second {
		t.Fatalf("while the cloud is slow = %v %v %v %v", v, oldAt, refreshing, err)
	}
	if _, _, refreshing, _ := cache.shown(context.Background(), 1, 0, 10*time.Millisecond, slow.fetch); !refreshing || slow.asked() != 1 {
		t.Fatalf("a second page started another read (%d) or lost the refresh", slow.asked())
	}
	close(slow.gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, newAt, refreshing, err := cache.shown(context.Background(), 1, time.Hour, 10*time.Millisecond, slow.fetch)
		if err == nil && len(v) == 2 && !refreshing && newAt.After(at) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new listing never replaced the old one: %v %v %v", v, refreshing, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A read that fails keeps the old listing shown, and the cloud is not asked
// again on every poll meanwhile; with no listing, the failure is the answer.
func TestShownListingKeepsTheOldOneWhenTheCloudFails(t *testing.T) {
	var cache listCache[[]string]
	good := &gatedFetch{value: []string{"c-1"}}
	if _, _, _, err := cache.shown(context.Background(), 1, time.Hour, time.Second, good.fetch); err != nil {
		t.Fatal(err)
	}
	broken := &gatedFetch{err: errors.New("ServiceUnavailable")}
	v, _, refreshing, err := cache.shown(context.Background(), 1, 0, time.Second, broken.fetch)
	if err != nil || len(v) != 1 || refreshing {
		t.Fatalf("a failed read = %v %v %v; want the old listing", v, refreshing, err)
	}
	if _, _, _, _ = cache.shown(context.Background(), 1, 0, time.Second, broken.fetch); broken.asked() != 1 {
		t.Fatalf("the cloud was asked %d times right after it failed", broken.asked())
	}
	var empty listCache[[]string]
	if _, _, _, err := empty.shown(context.Background(), 2, 0, time.Second, broken.fetch); err == nil || err.Error() != "ServiceUnavailable" {
		t.Fatalf("no listing and a failed read = %v", err)
	}
}

// With no listing yet, a page waits only as long as its own context.
func TestShownListingWaitsForTheFirstReadOnlyAsLongAsTheCaller(t *testing.T) {
	var cache listCache[[]string]
	slow := &gatedFetch{gate: make(chan struct{})}
	defer close(slow.gate)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, refreshing, err := cache.shown(ctx, 1, 0, time.Hour, slow.fetch); !errors.Is(err, ErrListingPending) || !refreshing {
		t.Fatalf("= %v %v; want ErrListingPending while the read goes on", refreshing, err)
	}
}
