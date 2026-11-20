//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

func TestNativeBeginObservationSeparatesCanceledAdmission(t *testing.T) {
	requireDatabaseHelper(t)
	db := contentionDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	holder, err := BeginWriterTransaction(ctx, db.SQL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	observed, o := ObserveTransactionBegin(ctx)
	waiting, stop := context.WithTimeout(observed, 100*time.Millisecond)
	before := db.SQL.Stats()
	_, err = BeginWriterTransaction(waiting, db.SQL, nil)
	stop()
	after := db.SQL.Stats()
	if !errors.Is(err, context.DeadlineExceeded) || before.InUse != after.InUse || before.WaitCount != after.WaitCount {
		t.Fatalf("queue cancellation changed pool/error: %v", err)
	}
	if d, n := o.Stage("admission_queue"); d < 90*time.Millisecond || n != 1 {
		t.Fatalf("queue duration/count=%s/%d", d, n)
	}
	for _, stage := range []string{"sql_begin_total", "driver_begin"} {
		if d, n := o.Stage(stage); d != -1 || n != 0 {
			t.Fatalf("canceled queue entered %s", stage)
		}
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	local, localO := ObserveTransactionBegin(ctx)
	tx, err := BeginWriterTransaction(local, db.Scheduling, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, n := localO.Stage("admission_queue"); n != 0 {
		t.Fatal("nonparticipating scheduling pool reported queue admission")
	}
	for _, stage := range []string{"sql_begin_total", "driver_begin"} {
		if d, n := localO.Stage(stage); d < 0 || n != 1 {
			t.Fatalf("local %s count=%d duration=%s", stage, n, d)
		}
	}
	// Leave a real BEGIN on the only idle wire, then observe the SDK rejecting
	// a nested BEGIN. No native result is stubbed; cleanup uses real ROLLBACK.
	if db.SQL.Stats().OpenConnections != 1 {
		t.Fatal("expected one native fixture wire")
	}
	if _, err := db.SQL.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	failed, failedO := ObserveTransactionBegin(ctx)
	_, err = BeginWriterTransaction(failed, db.SQL, nil)
	var nativeFailure dqlitedriver.Error
	if !errors.As(err, &nativeFailure) || nativeFailure.Code&0xff != 1 {
		t.Fatalf("want actual nested BEGIN error, got %v", err)
	}
	if _, err := db.SQL.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"admission_queue", "sql_begin_total", "driver_begin"} {
		if d, n := failedO.Stage(stage); d < 0 || n != 1 {
			t.Fatalf("failed %s count=%d duration=%s", stage, n, d)
		}
	}
	next, err := BeginWriterTransaction(ctx, db.SQL, nil)
	if err != nil {
		t.Fatal("failed begin retained writer", err)
	}
	if err := next.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Raw SQL deliberately bypasses writer admission and its outer Begin timer.
	rawCtx, rawO := ObserveTransactionBegin(ctx)
	raw, err := db.SQL.BeginTx(rawCtx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"admission_queue", "sql_begin_total"} {
		if _, calls := rawO.Stage(stage); calls != 0 {
			t.Fatalf("raw SQL entered %s", stage)
		}
	}
	if _, calls := rawO.Stage("driver_begin"); calls != 1 {
		t.Fatal("raw native BEGIN was not measured")
	}
	// Closed pool enters SQL Begin but never a driver, and still releases admission.
	closed := sql.OpenDB(db.SQL.Driver().(*transactionSQLConnector))
	_ = closed.Close()
	closedCtx, closedO := ObserveTransactionBegin(ctx)
	if _, err := BeginWriterTransaction(closedCtx, closed, nil); err == nil {
		t.Fatal("closed pool succeeded")
	}
	if _, n := closedO.Stage("driver_begin"); n != 0 {
		t.Fatal("closed pool invented driver invocation")
	}
	next, err = BeginWriterTransaction(ctx, db.SQL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Rollback(); err != nil {
		t.Fatal(err)
	}
}

type observedBeginConnector struct {
	driver.Connector
	delay time.Duration
	arm   *atomic.Bool
}

func (c *observedBeginConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if c.delay > 0 {
		timer := time.NewTimer(c.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	conn, err := c.Connector.Connect(ctx)
	if err == nil {
		c.arm.Store(true)
	}
	return conn, err
}

type observedBeginSocket struct {
	net.Conn
	arm      *atomic.Bool
	delay    time.Duration
	failOnce *atomic.Bool
}

func (c *observedBeginSocket) Write(p []byte) (int, error) {
	// The connector arms only after the real OPEN succeeds. The first subsequent
	// wire write belongs to actual SDK BEGIN; bytes and results are untouched.
	if c.arm.CompareAndSwap(true, false) {
		if c.failOnce != nil && c.failOnce.CompareAndSwap(true, false) {
			_ = c.Conn.Close()
			return 0, io.EOF
		}
		if c.delay > 0 {
			time.Sleep(c.delay)
		}
	}
	return c.Conn.Write(p)
}

func TestNativeBeginObservationSeparatesConnectionFromDriver(t *testing.T) {
	requireDatabaseHelper(t)
	for _, mode := range []string{"connection", "driver", "driver-retry"} {
		t.Run(mode, func(t *testing.T) {
			db := contentionDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			seeds := client.NewInmemNodeStore()
			if err := seeds.Set(ctx, []client.NodeInfo{{ID: db.ID, Address: db.Address}}); err != nil {
				t.Fatal(err)
			}
			var arm, failOnce atomic.Bool
			failOnce.Store(mode == "driver-retry")
			var connectionDelay, driverDelay time.Duration
			if mode == "connection" {
				connectionDelay = 200 * time.Millisecond
			} else if mode == "driver" {
				driverDelay = 200 * time.Millisecond
			}
			native, err := dqlitedriver.New(seeds, dqlitedriver.WithDialFunc(func(ctx context.Context, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
				if err != nil {
					return nil, err
				}
				return &observedBeginSocket{Conn: conn, arm: &arm, delay: driverDelay, failOnce: &failOnce}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			connector, err := native.OpenConnector("contention.db")
			if err != nil {
				t.Fatal(err)
			}
			pool := sql.OpenDB(&transactionSQLConnector{Connector: &observedBeginConnector{Connector: connector, delay: connectionDelay, arm: &arm}, writer: make(chan struct{}, 1)})
			defer pool.Close()
			observed, o := ObserveTransactionBegin(ctx)
			tx, err := BeginWriterTransaction(observed, pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			total, sqlCalls := o.Stage("sql_begin_total")
			driverTime, driverCalls := o.Stage("driver_begin")
			residual := total - driverTime
			wantDriverCalls := int64(1)
			if mode == "driver-retry" {
				wantDriverCalls = 2
			}
			if sqlCalls != 1 || driverCalls != wantDriverCalls || residual < 0 {
				t.Fatalf("counts/durations %d/%d %s/%s", sqlCalls, driverCalls, total, driverTime)
			}
			if mode == "connection" {
				if residual < connectionDelay || driverTime >= residual {
					t.Fatalf("connection delay misattributed total=%s driver=%s", total, driverTime)
				}
			} else if mode == "driver" {
				if driverTime < driverDelay || residual >= driverTime {
					t.Fatalf("driver delay misattributed total=%s driver=%s", total, driverTime)
				}
			}
			t.Logf("%s total=%s driver=%s pre_driver=%s counts=%d/%d", mode, total, driverTime, residual, sqlCalls, driverCalls)
		})
	}
}
