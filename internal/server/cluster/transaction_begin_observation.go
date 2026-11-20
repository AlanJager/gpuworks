package cluster

import (
	"context"
	"sync/atomic"
	"time"
)

type transactionBeginObservationKey struct{}

// TransactionBeginObservation records only completed timing stages, without
// callbacks, logging or SQL data. A request opts in before beginning work.
// Counts distinguish a stage never entered from zero-duration work or multiple
// driver attempts made by database/sql. Completion/authority is not inferred.
type TransactionBeginObservation struct {
	elapsed [3]atomic.Int64
	calls   [3]atomic.Int64
}

// ObserveTransactionBegin creates one diagnostic scope; child contexts retain
// it. No extra connection is acquired and no driver behavior is replaced.
func ObserveTransactionBegin(ctx context.Context) (context.Context, *TransactionBeginObservation) {
	observation := &TransactionBeginObservation{}
	return context.WithValue(ctx, transactionBeginObservationKey{}, observation), observation
}

// Stage returns the sum and actual invocation count for a fixed phase. An
// unobserved/unknown phase returns (-1,0), so callers cannot mistake absence
// for a measured instantaneous operation.
func (o *TransactionBeginObservation) Stage(phase string) (time.Duration, int64) {
	if o == nil {
		return -1, 0
	}
	index := transactionBeginPhase(phase)
	if index < 0 {
		return -1, 0
	}
	count := o.calls[index].Load()
	if count == 0 {
		return -1, 0
	}
	return time.Duration(o.elapsed[index].Load()), count
}

func transactionBeginPhase(phase string) int {
	switch phase {
	case "admission_queue":
		return 0
	case "sql_begin_total":
		return 1
	case "driver_begin":
		return 2
	default:
		return -1
	}
}

func observeTransactionBegin(ctx context.Context, phase string) func() {
	observation, _ := ctx.Value(transactionBeginObservationKey{}).(*TransactionBeginObservation)
	if observation == nil {
		return nil
	} // No clock read when instrumentation is off.
	index := transactionBeginPhase(phase)
	if index < 0 {
		return nil
	}
	started := time.Now()
	return func() { observation.elapsed[index].Add(int64(time.Since(started))); observation.calls[index].Add(1) }
}
