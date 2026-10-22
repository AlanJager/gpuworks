package hub

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
)

func delayedRPCHub(delay time.Duration) (*Hub, <-chan struct{}) {
	responded := make(chan struct{})
	session := &Session{pending: make(map[uint64]chan proto.Envelope), closed: make(chan struct{})}
	session.sendFn = func(_ string, id uint64, _ any) error {
		go func() {
			defer close(responded)
			time.Sleep(delay)
			session.dispatchRPCResp(proto.Envelope{ID: id, D: json.RawMessage(`{"result":{"complete":true,"candidates":[{"rel_path":"org/model"}]}}`)})
		}()
		return nil
	}
	return &Hub{sessions: map[string]*Session{"node": session}}, responded
}

func TestCallNodeReturnsScanAfterThirtySeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, responded := delayedRPCHub(40 * time.Second)
		defer func() { <-responded }()
		ctx, cancel := rpccontext.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		start := time.Now()
		raw, err := hub.CallNode(ctx, "node", proto.RPCStorageScan, proto.StorageScanParams{})
		if err != nil {
			t.Fatal(err)
		}
		var page proto.StorageScanResult
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if !page.Complete || len(page.Candidates) != 1 || page.Candidates[0].RelPath != "org/model" {
			t.Fatalf("lost scan result: %+v", page)
		}
		if elapsed := time.Since(start); elapsed != 40*time.Second {
			t.Fatalf("waited %s", elapsed)
		}
	})
}

func TestCallNodeKeepsOrdinaryTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, responded := delayedRPCHub(40 * time.Second)
		defer func() { <-responded }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		start := time.Now()
		_, err := hub.CallNode(ctx, "node", proto.RPCFSStat, nil)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 30*time.Second {
			t.Fatalf("ordinary RPC: elapsed=%s err=%v", time.Since(start), err)
		}
	})
}

func TestCallNodeHonoursExplicitBudgetAndParent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		parent time.Duration
		want   time.Duration
	}{
		{"budget", time.Minute, 2 * time.Minute, time.Minute},
		{"shorter parent", time.Minute, 5 * time.Second, 5 * time.Second},
		{"shorter budget", 5 * time.Second, time.Minute, 5 * time.Second},
		{"expired budget", 0, time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hub, responded := delayedRPCHub(90 * time.Second)
				defer func() { <-responded }()
				parent, cancelParent := context.WithTimeout(context.Background(), tc.parent)
				defer cancelParent()
				ctx, cancel := rpccontext.WithTimeout(parent, tc.budget)
				defer cancel()
				start := time.Now()
				_, err := hub.CallNode(ctx, "node", proto.RPCStorageScan, nil)
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != tc.want {
					t.Fatalf("elapsed=%s want=%s err=%v", time.Since(start), tc.want, err)
				}
				if pending := len(hub.session("node").pending); pending != 0 {
					t.Fatalf("pending RPCs after timeout=%d", pending)
				}
			})
		})
	}
}

func TestCallNodeExplicitBudgetPreservesCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, responded := delayedRPCHub(40 * time.Second)
		defer func() { <-responded }()
		parent, cancelParent := context.WithCancel(context.Background())
		defer cancelParent()
		ctx, cancel := rpccontext.WithTimeout(parent, time.Minute)
		defer cancel()
		go func() {
			time.Sleep(5 * time.Second)
			cancelParent()
		}()
		start := time.Now()
		_, err := hub.CallNode(ctx, "node", proto.RPCStorageScan, nil)
		if !errors.Is(err, context.Canceled) || time.Since(start) != 5*time.Second {
			t.Fatalf("elapsed=%s err=%v", time.Since(start), err)
		}
	})
}
