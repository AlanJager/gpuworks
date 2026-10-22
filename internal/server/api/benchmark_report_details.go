package api

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

// Report detail is optional and bounded independently of the complete point
// aggregates. Never follow a stored path outside the configured artifact root.
type benchmarkReportDetailBudget struct {
	bytes   int64
	records int
}

func newBenchmarkReportDetailBudget() *benchmarkReportDetailBudget {
	return &benchmarkReportDetailBudget{bytes: int64(benchmeta.MaxDetailCapMB) * 1024 * 1024, records: 10000}
}

func (a *apiHandlers) readBenchmarkReportDetails(ctx context.Context, run *store.BenchmarkRun, point store.BenchmarkPoint) ([]loadgen.DetailRecord, string, string) {
	return a.readBenchmarkReportDetailsWithin(ctx, run, point, newBenchmarkReportDetailBudget())
}

func (a *apiHandlers) readBenchmarkReportDetailsWithin(ctx context.Context, run *store.BenchmarkRun, point store.BenchmarkPoint, budget *benchmarkReportDetailBudget) (rows []loadgen.DetailRecord, status string, note string) {
	if !run.KeepDetail {
		return nil, "disabled", "Request detail retention was disabled."
	}
	if budget.bytes <= 0 || budget.records <= 0 {
		return nil, "truncated", "The shared report detail limit was reached; point aggregates remain complete."
	}
	if point.DetailPath == "" || a.d.BenchmarkArtifactDir == "" {
		return nil, "unavailable", "No retained request detail is available."
	}
	root, err := filepath.EvalSymlinks(a.d.BenchmarkArtifactDir)
	if err != nil {
		return nil, "unavailable", "The request detail directory is unavailable."
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, "unavailable", "The request detail directory is unavailable."
	}
	path, err := filepath.EvalSymlinks(point.DetailPath)
	if err != nil {
		return nil, "unavailable", "The request detail file is unavailable."
	}
	path, err = filepath.Abs(path)
	if err != nil || !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return nil, "unavailable", "The request detail path is outside the artifact directory."
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "unavailable", "The request detail file is unavailable."
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, "unavailable", "The compressed request detail is invalid."
	}
	defer gz.Close()
	capMB := run.DetailCapMB
	if capMB <= 0 {
		capMB = benchmeta.DefaultDetailCapMB
	}
	if capMB > benchmeta.MaxDetailCapMB {
		capMB = benchmeta.MaxDetailCapMB
	}
	limit := min(int64(capMB)*1024*1024, budget.bytes)
	limited := &io.LimitedReader{R: gz, N: limit + 1}
	defer func() {
		budget.bytes = max(0, budget.bytes-(limit+1-limited.N))
		budget.records = max(0, budget.records-len(rows))
	}()
	scan := bufio.NewScanner(limited)
	scan.Buffer(make([]byte, 4096), 4<<20)
	for scan.Scan() {
		if ctx.Err() != nil {
			return rows, "truncated", "Reading request detail was canceled."
		}
		var marker struct {
			Truncated bool `json:"truncated"`
		}
		if json.Unmarshal(scan.Bytes(), &marker) != nil {
			return rows, "truncated", "Request detail contains an invalid record."
		}
		if marker.Truncated || len(rows) >= budget.records || limited.N <= 0 {
			return rows, "truncated", "Only a bounded subset of request detail was retained."
		}
		var row loadgen.DetailRecord
		if json.Unmarshal(scan.Bytes(), &row) != nil {
			return rows, "truncated", "Request detail contains an invalid record."
		}
		if row.Version != 1 || row.RunID != run.ID || row.PointSeq != point.Seq || row.SessionID == "" || row.Turn < 1 {
			return rows, "unavailable", "Request detail has a legacy, unsupported, or mismatched identity."
		}
		rows = append(rows, row)
	}
	if scan.Err() != nil || limited.N <= 0 {
		return rows, "truncated", "Request detail is incomplete or exceeds the read limit."
	}
	if tail := benchmarkLongTailDTO(point.LongTail); tail != nil && tail.Evidence != nil && tail.Evidence.Version == 1 && len(rows) != tail.Evidence.All.Attempted {
		return rows, "truncated", "The retained request count differs from the measured request count."
	}
	return rows, "complete", ""
}
