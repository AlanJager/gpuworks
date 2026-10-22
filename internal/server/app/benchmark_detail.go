package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
)

// agentBenchmarkDetailSink appends already-compressed gzip JSONL chunks from
// an agent. Each chunk is bounded by the sender and the server enforces the
// run's configured cap before writing it to disk.
type agentBenchmarkDetailSink struct {
	root  string
	store *store.Store
	mu    sync.Mutex
	bad   map[string]bool
}

func newAgentBenchmarkDetailSink(root string, st *store.Store) *agentBenchmarkDetailSink {
	return &agentBenchmarkDetailSink{root: root, store: st, bad: map[string]bool{}}
}

func (d *agentBenchmarkDetailSink) Handle(nodeUUID string, st proto.BenchmarkStatus) {
	if d == nil || st.ID == 0 || st.PointIndex <= 0 {
		return
	}
	run, err := d.store.GetBenchmarkRun(st.ID)
	if err != nil || !run.KeepDetail {
		return
	}
	if run.LoadOrigin != nodeUUID {
		return
	}
	capMB := run.DetailCapMB
	if capMB <= 0 {
		capMB = benchmeta.DefaultDetailCapMB
	}
	if capMB > benchmeta.MaxDetailCapMB {
		capMB = benchmeta.MaxDetailCapMB
	}
	if err := os.MkdirAll(d.root, 0755); err != nil {
		return
	}
	path := filepath.Join(d.root, fmt.Sprintf("run-%d-point-%d.jsonl.gz", st.ID, st.PointIndex))
	d.mu.Lock()
	defer d.mu.Unlock()
	key := fmt.Sprintf("%d/%d", st.ID, st.PointIndex)
	if st.DetailSeq == 0 {
		_ = os.Remove(path)
		delete(d.bad, key)
	}
	if d.bad[key] {
		return
	}
	if len(st.DetailChunk) > 0 {
		info, e := os.Stat(path)
		if e == nil && info.Size()+int64(len(st.DetailChunk)) > int64(capMB)*1024*1024-4096 {
			d.bad[key] = true
			_ = os.Remove(path)
			return
		}
		f, e := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if e != nil {
			return
		}
		_, _ = f.Write(st.DetailChunk)
		_ = f.Close()
	}
	if st.DetailFinal {
		if d.bad[key] {
			return
		}
		_ = d.store.SetBenchmarkPointDetail(st.ID, st.PointIndex, path)
	}
}
