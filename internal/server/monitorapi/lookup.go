package monitorapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// LookupQuery is RequestSource.Lookup's query: one id, a window within
// uxrecord.MaxLookupWindow.
type LookupQuery struct {
	TraceID      tracecontext.TraceID
	RequestID    string
	FromMS, ToMS int64
	Limit        int
}

// Lookup windows. Neither id is indexed, so the window is what the lookup
// costs (ADR 0046).
const (
	// lookupDefaultWindow is searched, ending now, when the query names no
	// window and the trace id carries no plausible time.
	lookupDefaultWindow = time.Hour
	// A trace id the gateway started carries the millisecond it was minted,
	// a little after the request arrived (the gateway reads the body to
	// find the model first): search from a while before to a little after.
	lookupBeforeTraceTime = 10 * time.Minute
	lookupAfterTraceTime  = time.Minute
	// traceTimeHorizon is how far back a trace id's time is believed: past
	// the request store's retention (7 days) nothing is found anyway, and
	// an upstream's random id rarely decodes inside it.
	traceTimeHorizon = 8 * 24 * time.Hour
)

// Window sources (dto.MonitorRequestLookup.Window).
const (
	lookupWindowRequested = "requested"
	lookupWindowTraceTime = "trace_time"
	lookupWindowDefault   = "default"
)

// LookupRequests serves GET /monitor/requests/lookup. A query it refuses (no
// id or both, a malformed trace id, a window too long) comes back as
// Deps.BadQuery wrapped with the reason.
func (s *Service) LookupRequests(ctx context.Context, q dto.MonitorRequestLookupQuery) (dto.MonitorRequestLookup, error) {
	src := s.deps.Requests
	now := s.deps.now()
	traceText, requestID := strings.TrimSpace(q.TraceID), strings.TrimSpace(q.RequestID)
	if (traceText == "") == (requestID == "") {
		return dto.MonitorRequestLookup{}, fmt.Errorf("%w: give one of trace_id and request_id", s.badQuery())
	}
	var trace tracecontext.TraceID
	if traceText != "" {
		var ok bool
		if trace, ok = tracecontext.ParseTraceID(traceText); !ok {
			return dto.MonitorRequestLookup{}, fmt.Errorf("%w: trace_id must be 32 hex digits, not all zero", s.badQuery())
		}
	}
	if q.ToMS-q.FromMS > uxrecord.MaxLookupWindow.Milliseconds() {
		return dto.MonitorRequestLookup{}, fmt.Errorf("%w: the window is longer than %s", s.badQuery(), uxrecord.MaxLookupWindow)
	}
	out := dto.MonitorRequestLookup{AsOf: now.UnixMilli(), Requests: []dto.MonitorRequest{}, Deployments: []dto.MonitorLookupDeployment{}}
	if src == nil {
		return out, nil
	}

	type window struct {
		from, to int64
		source   string
	}
	var windows []window
	defaultWindow := window{now.Add(-lookupDefaultWindow).UnixMilli(), now.UnixMilli(), lookupWindowDefault}
	switch {
	case q.FromMS > 0:
		windows = []window{{q.FromMS, q.ToMS, lookupWindowRequested}}
	case !trace.IsZero() && plausibleTraceTime(trace.Time(), now):
		at := trace.Time()
		// An upstream's random id that happens to decode to a recent time
		// is not found there; the default window is searched next.
		windows = []window{{at.Add(-lookupBeforeTraceTime).UnixMilli(), at.Add(lookupAfterTraceTime).UnixMilli(), lookupWindowTraceTime}, defaultWindow}
	default:
		windows = []window{defaultWindow}
	}

	var records []uxrecord.Request
	used := windows[0]
	for _, w := range windows {
		found, err := src.Lookup(ctx, LookupQuery{TraceID: trace, RequestID: requestID, FromMS: w.from, ToMS: w.to, Limit: q.Limit})
		if errors.Is(err, ErrBadHistoryQuery) {
			return dto.MonitorRequestLookup{}, fmt.Errorf("%w: %w", s.badQuery(), err)
		}
		if err != nil {
			return dto.MonitorRequestLookup{}, err
		}
		used, records = w, found
		if len(found) > 0 {
			break
		}
	}
	out.FromMS, out.ToMS, out.Window = used.from, used.to, used.source

	names := src.KeyNames()
	var ids []uint
	for _, r := range records {
		out.Requests = append(out.Requests, recordDTO(r, names))
		if !slices.Contains(ids, r.DeploymentID) {
			ids = append(ids, r.DeploymentID)
		}
	}
	deployments := src.DeploymentNames(ids)
	for _, id := range ids {
		d := deployments[id]
		out.Deployments = append(out.Deployments, dto.MonitorLookupDeployment{ID: id, Name: d.Name, Deleted: d.Deleted})
	}
	return out, nil
}

// plausibleTraceTime reports whether a trace id's decoded time could be the
// gateway's: within the horizon and not in the future (beyond clock skew).
func plausibleTraceTime(at, now time.Time) bool {
	return !at.Before(now.Add(-traceTimeHorizon)) && !at.After(now.Add(lookupAfterTraceTime))
}
