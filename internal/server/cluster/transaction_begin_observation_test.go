package cluster

import (
	"context"
	"sync"
	"testing"
)

func TestTransactionBeginObservationOptInAndConcurrentCounts(t *testing.T) {
	if observeTransactionBegin(t.Context(), "driver_begin") != nil {
		t.Fatal("disabled diagnostic installed timing work")
	}
	ctx, o := ObserveTransactionBegin(t.Context())
	if d, n := o.Stage("driver_begin"); d != -1 || n != 0 {
		t.Fatal("unentered stage reported a duration")
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { done := observeTransactionBegin(ctx, "driver_begin"); done() })
	}
	wg.Wait()
	if d, n := o.Stage("driver_begin"); d < 0 || n != 12 {
		t.Fatalf("driver duration=%s attempts=%d", d, n)
	}
	if d, n := o.Stage("private-SQL-key"); d != -1 || n != 0 {
		t.Fatal("unknown stage was accepted")
	}
	child, cancel := context.WithCancel(ctx)
	cancel()
	done := observeTransactionBegin(child, "sql_begin_total")
	done()
	if _, n := o.Stage("sql_begin_total"); n != 1 {
		t.Fatal("canceled attempt was not recorded")
	}
}
