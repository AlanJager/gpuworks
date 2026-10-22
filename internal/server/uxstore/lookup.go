package uxstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Lookup bounds. The window bound is uxrecord.MaxLookupWindow, shared with
// the monitor API that chooses windows.
const (
	// DefaultLookupLimit and MaxLookupLimit bound the requests returned; a
	// trace can hold many requests (an agent's run is one trace).
	DefaultLookupLimit = 100
	MaxLookupLimit     = 500
)

// LookupQuery finds requests by an identifier the caller holds, across
// deployments: a trace id (every request of the trace) or a request id.
// Exactly one of the two is set.
type LookupQuery struct {
	TraceID   tracecontext.TraceID
	RequestID string
	// FromMS and ToMS bound ReceivedMS, inclusive; both are required and
	// at most uxrecord.MaxLookupWindow apart.
	FromMS, ToMS int64
	// Limit <= 0 is DefaultLookupLimit, capped at MaxLookupLimit.
	Limit int
}

// Lookup returns the requests matching q, newest first. Neither id has an
// index of its own (an index on a random key makes every insert dirty a page
// of its own; see partitionDDL): each day's query lists the deployments with
// traffic in the window from that day's count table (every request is
// counted there, sampled out of the log or not) and walks the
// (deployment_id, ts, seq) index over the window for each of them, checking
// the id on the rows it reaches. A request the log sampled away is not
// found; its counts are still in the distributions.
func (s *Store) Lookup(ctx context.Context, q LookupQuery) ([]uxrecord.Request, error) {
	hasTrace, hasID := !q.TraceID.IsZero(), strings.TrimSpace(q.RequestID) != ""
	switch {
	case hasTrace == hasID:
		return nil, fmt.Errorf("%w: give a trace id or a request id", ErrBadQuery)
	case q.FromMS <= 0 || q.ToMS < q.FromMS:
		return nil, fmt.Errorf("%w: lookup needs a time window", ErrBadQuery)
	case q.ToMS-q.FromMS > uxrecord.MaxLookupWindow.Milliseconds():
		return nil, fmt.Errorf("%w: lookup window is longer than %s", ErrBadQuery, uxrecord.MaxLookupWindow)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLookupLimit
	}
	limit = min(limit, MaxLookupLimit)
	match, value := "r.trace_id = ?", any(q.TraceID[:])
	if hasID {
		match, value = "r.id = ?", strings.TrimSpace(q.RequestID)
	}
	var out []uxrecord.Request
	// Partitions are days of ReceivedMS read newest first, so each day's
	// newest-first rows continue the order of the day before.
	err := s.eachDay(ctx, q.FromMS, q.ToMS, func(day string) (string, []any) {
		return lookupSQL(day, match), []any{floorBucket(q.FromMS), q.ToMS, q.FromMS, q.ToMS, value, limit - len(out)}
	}, func(rows *sql.Rows) error {
		var seq int64
		r, err := scanRequest(rows, &seq)
		if err != nil {
			return err
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

// lookupSQL is one day's lookup: the deployments counted in the window, then
// the _dep index walk of each over the window. The ORDER BY sorts only the
// matching rows.
func lookupSQL(day, match string) string {
	return fmt.Sprintf(`SELECT %s FROM %s r
		WHERE r.deployment_id IN (SELECT DISTINCT c.deployment_id FROM %s c WHERE c.bucket_ms >= ? AND c.bucket_ms <= ?)
		AND r.ts >= ? AND r.ts <= ? AND %s
		ORDER BY r.ts DESC, r.seq DESC LIMIT ?`, reqColumns, reqPrefix+day, cntPrefix+day, match)
}
