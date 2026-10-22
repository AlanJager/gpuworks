package controller

import (
	"testing"
	"time"
)

func TestWorkqCoalescesKeysAndHonorsEarliestDueTime(t *testing.T) {
	q := newWorkq(0)
	start := time.Now()
	q.addAfter("deployment/1", 25*time.Millisecond)
	q.addAfter("deployment/1", 100*time.Millisecond)
	q.addAfter("deployment/2", 5*time.Millisecond)
	q.mu.Lock()
	if len(q.pending) != 2 {
		t.Fatalf("pending keys = %d, want 2", len(q.pending))
	}
	q.mu.Unlock()
	key, ok := q.next()
	if !ok || key != "deployment/2" {
		t.Fatalf("first key = %q, ok=%v, want deployment/2", key, ok)
	}
	if elapsed := time.Since(start); elapsed < 3*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("first key elapsed = %v", elapsed)
	}
	key, ok = q.next()
	if !ok || key != "deployment/1" {
		t.Fatalf("second key = %q, ok=%v, want deployment/1", key, ok)
	}
	q.close()
}

func TestWorkqAddUsesDebounceAndAddAfterDelays(t *testing.T) {
	q := newWorkq(15 * time.Millisecond)
	start := time.Now()
	q.add("deployment/1")
	key, ok := q.next()
	if !ok || key != "deployment/1" {
		t.Fatalf("debounced key = %q, ok=%v", key, ok)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("debounce elapsed = %v, want at least 10ms", elapsed)
	}
	start = time.Now()
	q.addAfter("deployment/2", 20*time.Millisecond)
	key, ok = q.next()
	if !ok || key != "deployment/2" {
		t.Fatalf("delayed key = %q, ok=%v", key, ok)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Fatalf("addAfter elapsed = %v, want at least 15ms", elapsed)
	}
	q.close()
}

func TestWorkqClosePreventsDispatch(t *testing.T) {
	q := newWorkq(0)
	q.add("deployment/closed")
	q.close()
	if key, ok := q.next(); ok || key != "" {
		t.Fatalf("closed queue next = %q, %v; want empty,false", key, ok)
	}
	q.add("deployment/ignored")
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) != 1 {
		t.Fatalf("closed queue pending work changed: %+v", q.pending)
	}
	if _, ok := q.pending["deployment/ignored"]; ok {
		t.Fatal("closed queue accepted new pending work")
	}
}
