package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/benchrun"
	"gpuworks.ai/gpuworks/internal/proto"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

type benchEngine struct {
	mu         sync.Mutex
	desired    map[uint]proto.BenchmarkSpec
	executions map[uint]*benchExecution
	report     func(proto.BenchmarkStatus)
	log        *slog.Logger
}

type benchExecution struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newBenchEngine(report func(proto.BenchmarkStatus), log *slog.Logger) *benchEngine {
	return &benchEngine{desired: map[uint]proto.BenchmarkSpec{}, executions: map[uint]*benchExecution{}, report: report, log: log}
}
func (e *benchEngine) SetDesiredFull(ctx context.Context, specs []proto.BenchmarkSpec) {
	e.mu.Lock()
	wanted := map[uint]proto.BenchmarkSpec{}
	for _, s := range specs {
		wanted[s.ID] = s
	}
	for id, execution := range e.executions {
		if _, ok := wanted[id]; !ok {
			execution.cancel()
		}
	}
	e.desired = wanted
	e.mu.Unlock()
	for _, s := range specs {
		e.ensure(ctx, s)
	}
}
func (e *benchEngine) ApplyUpdate(ctx context.Context, specs []proto.BenchmarkSpec, removed []uint) {
	e.mu.Lock()
	for _, id := range removed {
		if execution := e.executions[id]; execution != nil {
			execution.cancel()
		}
		delete(e.desired, id)
	}
	e.mu.Unlock()
	for _, s := range specs {
		e.mu.Lock()
		e.desired[s.ID] = s
		e.mu.Unlock()
		e.ensure(ctx, s)
	}
}
func (e *benchEngine) ensure(parent context.Context, s proto.BenchmarkSpec) {
	e.mu.Lock()
	if _, wanted := e.desired[s.ID]; !wanted {
		e.mu.Unlock()
		return
	}
	if _, ok := e.executions[s.ID]; ok {
		e.mu.Unlock()
		return
	}
	ctx, c := context.WithCancel(parent)
	execution := &benchExecution{cancel: c, done: make(chan struct{})}
	e.executions[s.ID] = execution
	e.mu.Unlock()
	go func() {
		defer func() {
			close(execution.done)
			e.mu.Lock()
			if e.executions[s.ID] == execution {
				delete(e.executions, s.ID)
			}
			e.mu.Unlock()
		}()
		e.run(ctx, s)
	}()
}

// run executes one agent-origin run. Every status carries the warnings
// collected so far; per-request detail streams to the server after each
// point's result, so the point row exists when its detail arrives.
func (e *benchEngine) run(ctx context.Context, s proto.BenchmarkSpec) {
	var warnings []string
	report := func(status proto.BenchmarkStatus) {
		status.Warnings = warnings
		e.report(status)
	}
	// Only a run that keeps its detail needs a point's results back; without
	// it the point keeps none (see benchrun.Hooks.PointReported).
	var pointDetail func(int, []loadgen.Result)
	if s.KeepDetail {
		pointDetail = func(seq int, results []loadgen.Result) { e.reportDetail(s.ID, seq, s.DetailCapMB, results) }
	}
	benchrun.Execute(ctx, benchrun.Run{
		ID: s.ID, Model: s.Model, Mode: s.Mode, Dataset: s.Dataset, Search: s.Search, TokenizerRef: s.TokenizerRef,
		Client: loadgen.Client{HTTP: &http.Client{Timeout: 5 * time.Minute}, BaseURL: s.TargetURL, APIKey: s.APIKey},
	}, benchrun.Hooks{
		Report: report,
		Warn: func(code string) {
			if !containsString(warnings, code) {
				warnings = append(warnings, code)
			}
		},
		PointReported: pointDetail,
	})
	if ctx.Err() != nil {
		report(proto.BenchmarkStatus{ID: s.ID, State: "canceled", Message: ctx.Err().Error(), StopReason: proto.BenchmarkStopCanceled})
	}
}

func (e *benchEngine) Cancel(ctx context.Context, id uint) (bool, error) {
	e.mu.Lock()
	delete(e.desired, id)
	execution := e.executions[id]
	if execution != nil {
		execution.cancel()
	}
	e.mu.Unlock()
	if execution == nil {
		return false, nil
	}
	select {
	case <-execution.done:
		return true, nil
	case <-ctx.Done():
		return true, ctx.Err()
	}
}

func (e *benchEngine) reportDetail(runID uint, seq, capMB int, results []loadgen.Result) {
	if capMB <= 0 {
		capMB = benchmeta.DefaultDetailCapMB
	}
	if capMB > benchmeta.MaxDetailCapMB {
		capMB = benchmeta.MaxDetailCapMB
	}
	maxRaw := int64(capMB)*1024*1024 - 4096
	cw := &benchmarkChunkWriter{report: e.report, runID: runID, seq: seq}
	gz := gzip.NewWriter(cw)
	var written int64
	for _, r := range results {
		record := loadgen.NewDetailRecord(r)
		record.RunID, record.PointSeq = runID, seq
		line, _ := json.Marshal(record)
		line = append(line, '\n')
		if written+int64(len(line)) > maxRaw {
			_, _ = gz.Write([]byte("{\"truncated\":true}\n"))
			break
		}
		if _, err := gz.Write(line); err != nil {
			_ = gz.Close()
			return
		}
		written += int64(len(line))
	}
	if err := gz.Close(); err != nil {
		return
	}
	cw.finish()
}

const benchmarkDetailChunkSize = 64 * 1024

// benchmarkChunkWriter bounds memory use to one transport chunk while gzip is
// produced. This avoids materializing an entire per-request artifact in the
// agent before it can be sent.
type benchmarkChunkWriter struct {
	report    func(proto.BenchmarkStatus)
	runID     uint
	seq, part int
	buf       bytes.Buffer
}

func (w *benchmarkChunkWriter) Write(p []byte) (int, error) {
	original := len(p)
	_, _ = w.buf.Write(p)
	for w.buf.Len() >= benchmarkDetailChunkSize {
		chunk := make([]byte, benchmarkDetailChunkSize)
		_, _ = w.buf.Read(chunk)
		w.report(proto.BenchmarkStatus{ID: w.runID, State: "running", PointIndex: w.seq, DetailSeq: w.part, DetailChunk: chunk})
		w.part++
	}
	return original, nil
}

func (w *benchmarkChunkWriter) finish() {
	chunk := append([]byte(nil), w.buf.Bytes()...)
	w.report(proto.BenchmarkStatus{ID: w.runID, State: "running", PointIndex: w.seq, DetailSeq: w.part, DetailChunk: chunk, DetailFinal: true})
}
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
