// Package apiwire decodes the management API's shared response envelopes for
// test clients. It depends only on the standard library so harness binaries
// (fleet hosts, L4 clients) can use it without pulling in testenv.
package apiwire

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// List is the management API's collection envelope, {"items": [...],
// "total": N} (docs/reference/api-conventions.md). Decoding is strict on
// purpose: a bare array, a missing or null "items", a missing "total", a total
// that disagrees with len(items), or an unexpected top-level key is an error,
// so a suite never silently accepts the pre-envelope wire shape.
type List[T any] struct {
	Items []T
	Total int
}

func (l *List[T]) UnmarshalJSON(data []byte) error {
	fields, err := envelopeFields(data, []string{"items", "total"})
	if err != nil {
		return err
	}
	items, err := decodeItems[T](fields["items"])
	if err != nil {
		return err
	}
	var total int
	if err := json.Unmarshal(fields["total"], &total); err != nil {
		return fmt.Errorf("list envelope: total: %w", err)
	}
	if total != len(items) {
		return fmt.Errorf("list envelope: total=%d but %d item(s)", total, len(items))
	}
	l.Items, l.Total = items, total
	return nil
}

// CursorPage is the envelope of a cursor-paged timeline (instance and
// deployment events): {"items": [...]} with no total.
type CursorPage[T any] struct {
	Items []T
}

func (p *CursorPage[T]) UnmarshalJSON(data []byte) error {
	fields, err := envelopeFields(data, []string{"items"})
	if err != nil {
		return err
	}
	items, err := decodeItems[T](fields["items"])
	if err != nil {
		return err
	}
	p.Items = items
	return nil
}

// DecodeList strictly decodes a {"items", "total"} body and returns its items.
func DecodeList[T any](data []byte) ([]T, error) {
	var list List[T]
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// DecodeCursorPage strictly decodes a {"items"} body and returns its items.
func DecodeCursorPage[T any](data []byte) ([]T, error) {
	var page CursorPage[T]
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, err
	}
	return page.Items, nil
}

// UnmarshalList strictly decodes a {"items", "total"} body into *out, for
// call sites that already hold a typed slice.
func UnmarshalList[T any](data []byte, out *[]T) error {
	items, err := DecodeList[T](data)
	if err != nil {
		return err
	}
	*out = items
	return nil
}

// UnmarshalCursorPage strictly decodes a {"items"} body into *out.
func UnmarshalCursorPage[T any](data []byte, out *[]T) error {
	items, err := DecodeCursorPage[T](data)
	if err != nil {
		return err
	}
	*out = items
	return nil
}

// ReadList reads r to the end and strictly decodes it as a {"items", "total"}
// body into *out.
func ReadList[T any](r io.Reader, out *[]T) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return UnmarshalList(data, out)
}

// ReadCursorPage reads r to the end and strictly decodes it as a {"items"}
// body into *out.
func ReadCursorPage[T any](r io.Reader, out *[]T) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return UnmarshalCursorPage(data, out)
}

// MetricPoint is one [unix ms, value] pair of /metrics/query and
// /monitor/series.
type MetricPoint struct {
	TimeMS int64
	Value  float64
}

func (p *MetricPoint) UnmarshalJSON(data []byte) error {
	var pair []json.Number
	if err := json.Unmarshal(data, &pair); err != nil {
		return fmt.Errorf("metric point %s: want [t_ms, value]: %w", data, err)
	}
	if len(pair) != 2 {
		return fmt.Errorf("metric point %s: want exactly [t_ms, value]", data)
	}
	t, err := pair[0].Int64()
	if err != nil {
		return fmt.Errorf("metric point %s: time: %w", data, err)
	}
	v, err := pair[1].Float64()
	if err != nil {
		return fmt.Errorf("metric point %s: value: %w", data, err)
	}
	p.TimeMS, p.Value = t, v
	return nil
}

// MetricSeries is one element of /metrics/query and /monitor/series.
type MetricSeries struct {
	Labels map[string]string `json:"labels"`
	Points []MetricPoint     `json:"points"`
}

// envelopeFields splits a JSON object into its top-level fields, requiring
// every key in required and rejecting any key outside required and optional.
func envelopeFields(data []byte, required []string, optional ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("want a {%s} envelope: %w", strings.Join(required, ", "), err)
	}
	if fields == nil {
		return nil, fmt.Errorf("want a {%s} envelope, got null", strings.Join(required, ", "))
	}
	want := map[string]bool{}
	for _, k := range optional {
		want[k] = true
	}
	for _, k := range required {
		want[k] = true
		if _, ok := fields[k]; !ok {
			return nil, fmt.Errorf("envelope is missing %q", k)
		}
	}
	var extra []string
	for k := range fields {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Errorf("envelope has unexpected key(s) %v", extra)
	}
	return fields, nil
}

func decodeItems[T any](raw json.RawMessage) ([]T, error) {
	if string(raw) == "null" {
		return nil, fmt.Errorf(`envelope "items" is null; an empty collection is []`)
	}
	var items []T
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("envelope items: %w", err)
	}
	return items, nil
}

// LogEntry is one line of /logs/search and /logs/context.
type LogEntry struct {
	ID       int64  `json:"id"`
	Instance string `json:"instance"`
	Node     string `json:"node"`
	Level    string `json:"level"`
	Source   string `json:"source"`
	TimeMS   int64  `json:"time_ms"`
	Line     string `json:"line"`
}

// LogsPage is the /logs/search body: {"items": [...]} plus the omitempty
// "total", "total_capped" and "truncated". "items" is required and no other
// top-level key is accepted.
type LogsPage struct {
	Items       []LogEntry
	Total       int64
	TotalCapped bool
	Truncated   bool
}

func (p *LogsPage) UnmarshalJSON(data []byte) error {
	fields, err := envelopeFields(data, []string{"items"}, "total", "total_capped", "truncated")
	if err != nil {
		return err
	}
	items, err := decodeItems[LogEntry](fields["items"])
	if err != nil {
		return err
	}
	*p = LogsPage{Items: items}
	for key, dst := range map[string]any{"total": &p.Total, "total_capped": &p.TotalCapped, "truncated": &p.Truncated} {
		if raw, ok := fields[key]; ok {
			if err := json.Unmarshal(raw, dst); err != nil {
				return fmt.Errorf("logs page %s: %w", key, err)
			}
		}
	}
	return nil
}

// LogsWindow is the /logs/context body: {"items": [...], "match_index": N}.
type LogsWindow struct {
	Items      []LogEntry
	MatchIndex int
}

func (w *LogsWindow) UnmarshalJSON(data []byte) error {
	fields, err := envelopeFields(data, []string{"items", "match_index"})
	if err != nil {
		return err
	}
	items, err := decodeItems[LogEntry](fields["items"])
	if err != nil {
		return err
	}
	var match int
	if err := json.Unmarshal(fields["match_index"], &match); err != nil {
		return fmt.Errorf("logs window match_index: %w", err)
	}
	*w = LogsWindow{Items: items, MatchIndex: match}
	return nil
}

// LogsBucket is one element of the /logs/histogram list.
type LogsBucket struct {
	StartMS int64            `json:"start_ms"`
	Counts  map[string]int64 `json:"counts"`
}
