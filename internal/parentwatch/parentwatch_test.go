package parentwatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestFollowCallsGoneOnceTheParentChanges(t *testing.T) {
	var parent atomic.Int64
	parent.Store(42)
	done := make(chan struct{})
	go follow(context.Background(), 42, func() { close(done) }, func() int { return int(parent.Load()) }, time.Millisecond)
	select {
	case <-done:
		t.Fatal("gone called while the parent lives")
	case <-time.After(20 * time.Millisecond):
	}
	parent.Store(1) // reparented to init
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gone not called after the parent left")
	}
}

func TestFollowReturnsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		follow(ctx, 42, func() { t.Error("gone called") }, func() int { return 42 }, time.Millisecond)
		close(returned)
	}()
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("follow did not return")
	}
}

func TestFollowNoticesAParentAlreadyGone(t *testing.T) {
	called := false
	follow(context.Background(), 42, func() { called = true }, func() int { return 1 }, time.Hour)
	if !called {
		t.Fatal("a parent gone before follow started was not noticed")
	}
}
