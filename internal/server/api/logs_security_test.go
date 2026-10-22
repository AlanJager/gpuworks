package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/logstore"
)

func TestLogsRejectUnsafeQueriesWithRealStore(t *testing.T) {
	h := newLogsHarness(t)
	logs, err := logstore.OpenSQLite(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close(); _ = h.store.Close() })
	if err := logs.Append([]logstore.Entry{{TimeMS: time.Now().UnixMilli(), Instance: "instance", Source: "instance", Level: "info", Line: "null log entry"}}); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	h.router = Router(Deps{Store: h.store, AuthSigner: h.signer, Log: slog.New(slog.NewTextHandler(&logged, nil)),
		LogsSearch: func(f LogsFilter) (LogsPage, error) {
			result, err := logs.Search(logstore.Filter{Query: f.Query, Source: f.Source})
			page := LogsPage{}
			for _, entry := range result.Entries {
				page.Items = append(page.Items, LogEntry{ID: entry.ID, Instance: entry.Instance, Node: entry.Node, Level: entry.Level, Source: entry.Source, TimeMS: entry.TimeMS, Line: entry.Line})
			}
			return page, err
		},
		LogsHistogram: func(f LogsFilter, bucket int64) ([]LogsBucket, error) {
			buckets, err := logs.Histogram(logstore.Filter{Query: f.Query, Source: f.Source}, bucket)
			var result []LogsBucket
			for _, bucket := range buckets {
				result = append(result, LogsBucket{StartMS: bucket.StartMS, Counts: bucket.Counts})
			}
			return result, err
		},
	})
	for _, endpoint := range []string{"search", "histogram"} {
		for _, query := range []string{"\x00null", "null\x00", "bad\nquery", "bad\x7fquery", strings.Repeat("a", 4097)} {
			code, raw := h.getRaw(t, "/api/v1/logs/"+endpoint+"?q="+url.QueryEscape(query))
			if code != http.StatusBadRequest || !strings.Contains(raw, `"code":"FIELD_INVALID"`) {
				t.Errorf("%s query length %d: got %d %s", endpoint, len(query), code, raw)
			}
			if strings.Contains(raw, "SQL") {
				t.Errorf("backend details leaked: %s", raw)
			}
		}
	}
	if code, raw := h.getRaw(t, "/api/v1/logs/search?q=null"); code != http.StatusOK || !strings.Contains(raw, `"line":"null log entry"`) {
		t.Fatalf("valid search: %d %s", code, raw)
	}
	if code, raw := h.getRaw(t, "/api/v1/logs/histogram?q=null"); code != http.StatusOK || !strings.Contains(raw, `"info":1`) {
		t.Fatalf("valid histogram: %d %s", code, raw)
	}
	// A backend failure stays diagnosable in server logs, but SQL details are not sent to callers.
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"search", "histogram"} {
		code, raw := h.getRaw(t, "/api/v1/logs/"+endpoint+"?q=null")
		if code != http.StatusInternalServerError || strings.Contains(raw, "database") || strings.Contains(raw, "SQL") {
			t.Errorf("%s storage failure: %d %s", endpoint, code, raw)
		}
	}
	if !strings.Contains(logged.String(), "database is closed") {
		t.Fatalf("backend error missing from server log: %s", &logged)
	}
}
