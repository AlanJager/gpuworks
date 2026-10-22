package distribution

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func indexRequests(s *objectStore, channel string) int {
	n := 0
	for _, r := range s.requestLog() {
		if strings.HasPrefix(r, channel+"/index.json") {
			n++
		}
	}
	return n
}

// Concurrent probes share one request, and a fresh probe is reused until a
// manual check asks again.
func TestCheckCacheProbeReusesAFreshCheck(t *testing.T) {
	store := newObjectStore(t)
	publish(t, testIndex("nightly"), store)
	store.fail("nightly/index.json", fault{delay: 100 * time.Millisecond})
	client := clientFor(store)
	var cache CheckCache
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if fetched, err, _ := cache.Probe(context.Background(), client, "nightly", false, nil); err != nil || fetched.Index.Server.ReleaseID != "abc1234" {
				t.Errorf("probe = %+v, %v", fetched, err)
			}
		}()
	}
	wg.Wait()
	if got := indexRequests(store, "nightly"); got != 1 {
		t.Fatalf("five probes asked %d times, want once", got)
	}
	if _, _, at := cache.Probe(context.Background(), client, "nightly", true, nil); at.IsZero() || indexRequests(store, "nightly") != 2 {
		t.Fatalf("a refresh did not ask again: %d requests", indexRequests(store, "nightly"))
	}
	// Another channel is another check.
	cache.Probe(context.Background(), client, "release", false, nil)
	if got := indexRequests(store, "release"); got != 1 {
		t.Fatalf("release asked %d times", got)
	}
}

// A caller that goes away does not record the sources as failed.
func TestCheckCacheIgnoresACancelledProbe(t *testing.T) {
	store := newObjectStore(t)
	publish(t, testIndex("nightly"), store)
	store.fail("nightly/index.json", fault{delay: time.Second, once: true})
	client := clientFor(store)
	var cache CheckCache
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err, _ := cache.Probe(ctx, client, "nightly", false, nil); err == nil {
		t.Fatal("a cancelled probe succeeded")
	}
	if fetched, err, _ := cache.Probe(context.Background(), client, "nightly", false, nil); err != nil || fetched == nil {
		t.Fatalf("the probe after a cancelled one = %+v, %v", fetched, err)
	}
}

// Prefetch returns at once, runs one probe at a time, clears its mark when
// done, and asks a failed channel again only after FailedCheckTTL.
func TestCheckCachePrefetch(t *testing.T) {
	store := newObjectStore(t)
	publish(t, testIndex("nightly"), store)
	store.fail("nightly/index.json", fault{delay: 200 * time.Millisecond})
	client := clientFor(store)
	var cache CheckCache
	waitIdle := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for cache.prefetching.Load() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if cache.prefetching.Load() {
			t.Fatal("the prefetch never finished")
		}
	}
	start := time.Now()
	for i := 0; i < 5; i++ {
		cache.Prefetch(client, "nightly", time.Minute, nil)
	}
	if waited := time.Since(start); waited > 150*time.Millisecond {
		t.Fatalf("prefetch waited %s for the sources", waited)
	}
	if !cache.prefetching.Load() {
		t.Fatal("no prefetch is running")
	}
	waitIdle()
	if got := indexRequests(store, "nightly"); got != 1 {
		t.Fatalf("five prefetches asked the sources %d times, want once", got)
	}
	// Fresh: nothing to do.
	cache.Prefetch(client, "nightly", time.Minute, nil)
	waitIdle()
	if got := indexRequests(store, "nightly"); got != 1 {
		t.Fatalf("a fresh check was asked again: %d requests", got)
	}

	cache.mu.Lock()
	cache.err, cache.at = errors.New("every source failed"), time.Now().UTC().Add(-FailedCheckTTL+time.Minute)
	cache.mu.Unlock()
	cache.Prefetch(client, "nightly", time.Minute, nil)
	waitIdle()
	if got := indexRequests(store, "nightly"); got != 1 {
		t.Fatalf("a failure inside its retry window was asked again: %d requests", got)
	}
	cache.mu.Lock()
	cache.at = time.Now().UTC().Add(-FailedCheckTTL - time.Second)
	cache.mu.Unlock()
	cache.Prefetch(client, "nightly", time.Minute, nil)
	waitIdle()
	if got := indexRequests(store, "nightly"); got != 2 {
		t.Fatalf("a failure past its retry window was not asked again: %d requests", got)
	}
}
