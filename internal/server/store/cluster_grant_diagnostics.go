package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"log/slog"
	"net"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// Diagnostics are summaries, never per-query logging. The threshold affects
// logging only; it does not change an authorization deadline or retry policy.
const clusterGrantDiagnosticSlow = 250 * time.Millisecond

// SetLogger binds application diagnostics before the Store is shared with
// workers or scoped copies. It must not be called on a running Store.
func (s *Store) SetLogger(log *slog.Logger) {
	s.diagnosticLogger = log
}

func (s *Store) logClusterGrantDiagnostic(ctx context.Context, operation, phase string, started time.Time, err error, timings ...slog.Attr) {
	elapsed := time.Since(started)
	if err == nil && elapsed < clusterGrantDiagnosticSlow {
		return
	}
	log := s.clusterGrantDiagnosticLogger()
	if !log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	attrs := []slog.Attr{
		slog.String("operation", operation), slog.String("phase", phase),
		clusterGrantTiming("elapsed_ms", elapsed),
		slog.String("err_class", clusterGrantErrorClass(err)),
		// This is the supplied context, not a nested transaction context whose
		// deferred cancellation has already run when the transaction returns.
		slog.String("caller_context_class", clusterGrantErrorClass(ctx.Err())),
	}
	log.LogAttrs(ctx, slog.LevelDebug, "cluster grant diagnostic", append(attrs, timings...)...)
}

// A negative duration means the stage was never entered, so omit the attribute.
func clusterGrantTiming(name string, elapsed time.Duration) slog.Attr {
	if elapsed < 0 {
		return slog.Attr{}
	}
	return slog.Float64(name, float64(elapsed)/float64(time.Millisecond))
}

// Errors can contain credentials, SQL arguments and authority paths. Preserve
// the original error for the caller and expose only fixed classifications here.
func clusterGrantErrorClass(err error) string {
	var timeout net.Error
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, driver.ErrBadConn):
		return "bad_connection"
	case errors.Is(err, ErrClusterAdmissionRejected):
		return "admission_rejected"
	case errors.Is(err, ErrClusterIdentityConflict):
		return "identity_conflict"
	case errors.Is(err, ErrMaintenanceNotServing):
		return "maintenance_not_serving"
	case errors.Is(err, ErrBusinessWritesClosed):
		return "business_writes_closed"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	default:
		return "other"
	}
}

func (s *Store) clusterGrantDiagnosticLogger() *slog.Logger {
	if s.diagnosticLogger != nil {
		return s.diagnosticLogger
	}
	return slog.Default()
}

type clusterGrantPurposeKey struct{}

func clusterBeginDiagnosticTimings(ctx context.Context, observation *cluster.TransactionBeginObservation) []slog.Attr {
	purpose, _ := ctx.Value(clusterGrantPurposeKey{}).(string)
	switch purpose {
	case "transport_admissions", "private_material_grant":
	default:
		purpose = "cluster_write"
	}
	attrs := []slog.Attr{slog.String("purpose", purpose)}
	for _, stage := range []string{"admission_queue", "sql_begin_total", "driver_begin"} {
		elapsed, calls := observation.Stage(stage)
		attrs = append(attrs, slog.Int64(stage+"_calls", calls), clusterGrantTiming(stage+"_ms", elapsed))
	}
	total, totalCalls := observation.Stage("sql_begin_total")
	driver, driverCalls := observation.Stage("driver_begin")
	if totalCalls > 0 && driverCalls > 0 {
		attrs = append(attrs, clusterGrantTiming("pre_driver_ms", max(total-driver, 0)))
	}
	return attrs
}
