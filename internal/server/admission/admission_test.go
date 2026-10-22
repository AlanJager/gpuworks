package admission

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

type recordingPublisher struct {
	mu    sync.Mutex
	items []bus.Event
	order *[]string
}

func (p *recordingPublisher) Publish(event bus.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = append(p.items, event)
	if p.order != nil {
		*p.order = append(*p.order, "publish:"+event.Key)
	}
}

type recordingQueue struct {
	mu    sync.Mutex
	keys  []string
	order *[]string
}

func (q *recordingQueue) Enqueue(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.keys = append(q.keys, key)
	if q.order != nil {
		*q.order = append(*q.order, "enqueue:"+key)
	}
}

func TestGateSerializesConcurrentTransactions(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var mu sync.Mutex
	active, maxActive := 0, 0
	transaction := func(ctx context.Context, fn func(context.Context) (Effects, error)) error {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()
		if _, err := fn(ctx); err != nil {
			return err
		}
		startedOnce.Do(func() { close(started) })
		<-release
		return nil
	}
	gate, err := New(transaction, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- gate.Run(context.Background(), func(context.Context) (Effects, error) { return Effects{}, nil })
	}()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- gate.Run(context.Background(), func(context.Context) (Effects, error) { return Effects{}, nil })
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second transaction completed while first was active: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("max concurrent transactions=%d, want 1", maxActive)
	}
}

func TestGateDoesNotPublishOrEnqueueOnTransactionError(t *testing.T) {
	var order []string
	publisher := &recordingPublisher{order: &order}
	queue := &recordingQueue{order: &order}
	wantErr := errors.New("rollback")
	gate, err := New(func(ctx context.Context, fn func(context.Context) (Effects, error)) error {
		if _, err := fn(ctx); err != nil {
			return err
		}
		return wantErr
	}, publisher, queue)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Run(context.Background(), func(context.Context) (Effects, error) {
		return Effects{Events: []bus.Event{{Topic: "instance", Key: "i1"}}, Enqueue: []string{"d1"}}, nil
	}); !errors.Is(err, wantErr) {
		t.Fatalf("error=%v, want %v", err, wantErr)
	}
	if len(order) != 0 {
		t.Fatalf("post-rollback effects=%v", order)
	}
}

func TestGateDispatchesOnlyAfterCommitInStableOrder(t *testing.T) {
	var order []string
	publisher := &recordingPublisher{order: &order}
	queue := &recordingQueue{order: &order}
	gate, err := New(func(ctx context.Context, fn func(context.Context) (Effects, error)) error {
		order = append(order, "commit:begin")
		effects, err := fn(ctx)
		if err != nil {
			return err
		}
		order = append(order, "commit:end")
		_ = effects
		return nil
	}, publisher, queue)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Run(context.Background(), func(context.Context) (Effects, error) {
		order = append(order, "callback")
		return Effects{
			Events:  []bus.Event{{Topic: "instance", Key: "i1"}, {Topic: "deployment", Key: "d1"}},
			Enqueue: []string{"i1", "d1"},
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"commit:begin", "callback", "commit:end", "publish:i1", "publish:d1", "enqueue:i1", "enqueue:d1"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order=%v, want %v", order, want)
	}
}

func TestGatePreservesOpaqueTransactionHandleForStoreAdapter(t *testing.T) {
	type fakeTx struct{ id string }
	handle := &fakeTx{id: "tx-1"}
	gate, err := New(func(ctx context.Context, fn func(context.Context) (Effects, error)) error {
		return func() error {
			txCtx := WithTransactionHandle(ctx, handle)
			_, err := fn(txCtx)
			return err
		}()
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Run(context.Background(), func(ctx context.Context) (Effects, error) {
		got, ok := TransactionHandle(ctx)
		if !ok || got.(*fakeTx) != handle {
			t.Fatalf("transaction handle=%v, ok=%v", got, ok)
		}
		return Effects{}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGateCallbackIsTheOnlyTransactionWorkAndNoExternalIOMock(t *testing.T) {
	var callbacks int
	gate, err := New(func(ctx context.Context, fn func(context.Context) (Effects, error)) error {
		callbacks++
		_, err := fn(ctx)
		return err
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.Run(context.Background(), func(context.Context) (Effects, error) {
		return Effects{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if callbacks != 1 {
		t.Fatalf("transaction callback count=%d, want 1", callbacks)
	}
}
