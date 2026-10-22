package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"time"
)

// stallDumper answers the question the aggregate counters cannot: when a live
// node stops being heard from, what is the process actually doing? It watches
// heartbeat age and, the moment a node crosses the trigger, writes a full
// goroutine dump (plus mutex and block profiles when they are enabled) so the
// blocking stack is captured while the stall is developing rather than after
// the session has already been torn down and replaced.
//
// It is off unless GPUWORKS_FLEET_DUMP_DIR names a directory, so the load case
// itself is unchanged when nobody is diagnosing it.
type stallDumper struct {
	dir          string
	stallTrigger time.Duration
	taken        atomic.Int32
	limit        int32
}

func newStallDumper() *stallDumper {
	dir := os.Getenv("GPUWORKS_FLEET_DUMP_DIR")
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	return &stallDumper{
		dir:          dir,
		stallTrigger: time.Duration(EnvInt("GPUWORKS_FLEET_DUMP_TRIGGER_MS", 40000)) * time.Millisecond,
		limit:        int32(EnvInt("GPUWORKS_FLEET_DUMP_LIMIT", 6)),
	}
}

// onStall adapts the dumper to the watch's hook, and is nil when no directory
// was named so the watch does no extra work in an ordinary run.
func (d *stallDumper) onStall() func(string) {
	if d == nil {
		return nil
	}
	return d.dump
}

func (d *stallDumper) trigger() time.Duration {
	if d == nil {
		return 0
	}
	return d.stallTrigger
}

func (d *stallDumper) dump(label string) {
	if d == nil || d.taken.Add(1) > d.limit {
		return
	}
	stamp := time.Now().Format("150405.000")
	file, err := os.Create(filepath.Join(d.dir, fmt.Sprintf("%s-%s.txt", stamp, label)))
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "goroutines=%d\n\n", runtime.NumGoroutine())
	_ = pprof.Lookup("goroutine").WriteTo(file, 2)
	if p := pprof.Lookup("mutex"); p != nil {
		fmt.Fprintln(file, "\n==== mutex ====")
		_ = p.WriteTo(file, 1)
	}
	if p := pprof.Lookup("block"); p != nil {
		fmt.Fprintln(file, "\n==== block ====")
		_ = p.WriteTo(file, 1)
	}
}

// enableContentionProfiling turns on the mutex and block profilers when the
// dumper is active. Both sample, so they are only paid for while diagnosing.
func enableContentionProfiling(d *stallDumper) {
	if d == nil {
		return
	}
	runtime.SetMutexProfileFraction(EnvInt("GPUWORKS_FLEET_MUTEX_FRACTION", 100))
	runtime.SetBlockProfileRate(EnvInt("GPUWORKS_FLEET_BLOCK_RATE_NS", 1000000))
}
