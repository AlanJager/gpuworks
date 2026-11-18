package pluginsdk

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// A plugin whose parent is gone (reparented, as on macOS where no
// parent-death signal exists) notices within a poll.
func TestWatchParentNoticesReparenting(t *testing.T) {
	var ppid atomic.Int64
	ppid.Store(4242)
	old := getppid
	getppid = func() int { return int(ppid.Load()) }
	defer func() { getppid = old }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gone := watchParent(ctx, 5*time.Millisecond)
	select {
	case <-gone:
		t.Fatal("the parent is still there")
	case <-time.After(30 * time.Millisecond):
	}
	ppid.Store(1)
	select {
	case <-gone:
	case <-time.After(2 * time.Second):
		t.Fatal("reparenting went unnoticed")
	}
}

func TestNegotiate(t *testing.T) {
	offer := map[string]extapi.Range{"a": {Min: 1, Max: 3}, "b": {Min: 2, Max: 2}, "c": {Min: 1, Max: 1}}
	use, err := negotiate(offer,
		map[string]extapi.Range{"a": {Min: 1, Max: 2}},
		map[string]extapi.Range{"b": {Min: 1, Max: 1}, "c": {Min: 1, Max: 4}, "d": {Min: 1, Max: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(use) != 2 || use["a"] != 2 || use["c"] != 1 {
		t.Fatalf("use = %v, want a@2 and c@1 (b's versions do not meet, d is not offered)", use)
	}
	if _, err := negotiate(offer, map[string]extapi.Range{"b": {Min: 3, Max: 4}}, nil); err == nil || err.Code != extapi.ErrorCapabilityMissing {
		t.Fatalf("required b@3..4 against b@2 = %v", err)
	} else if err.Params["capability"] != "b" || err.Params["min"] != 3 || err.Params["max"] != 4 {
		t.Fatalf("params = %v", err.Params)
	}
}
