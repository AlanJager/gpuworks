package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// stallOutputLines is how much of the output's end is compared between
// samples: any new line changes it.
const stallOutputLines = 20

// stallSampleTimeout bounds one observation; what does not answer in time is
// unknown for that sample.
const stallSampleTimeout = 20 * time.Second

// procFS is how the watch reads /proc; the zero value reads the host's.
type procFS struct {
	root     string
	readDir  func(string) ([]os.DirEntry, error)
	readFile func(string) ([]byte, error)
}

func (f procFS) orHost() procFS {
	if f.root == "" {
		f.root = "/proc"
	}
	if f.readDir == nil {
		f.readDir = os.ReadDir
	}
	if f.readFile == nil {
		f.readFile = os.ReadFile
	}
	return f
}

// watchesStartStall reports whether a starting instance is watched for a
// stall: one that owns cards and is not a rank of a multi-node unit, on a
// runtime that can name its processes.
func (e *Engine) watchesStartStall(spec proto.InstanceDesiredState) bool {
	if spec.Headless || spec.NNodes > 1 || len(spec.GPUIndexes) == 0 || e.prober == nil {
		return false
	}
	_, ok := e.Driver().(runtime.ProcessRooter)
	return ok
}

// sampleStartStall observes a starting instance once, without its cards.
func (e *Engine) sampleStartStall(ctx context.Context, spec proto.InstanceDesiredState, id string) stallSample {
	ctx, cancel := context.WithTimeout(ctx, stallSampleTimeout)
	defer cancel()
	s := stallSample{peerBusy: e.otherInstanceInFlight(spec.UUID)}
	if rc, err := e.Driver().Logs(ctx, id, false, stallOutputLines); err == nil {
		data, readErr := io.ReadAll(io.LimitReader(rc, 256<<10))
		_ = rc.Close()
		if readErr == nil {
			sum := sha256.Sum256(data)
			s.outputKnown, s.output = true, hex.EncodeToString(sum[:8])
		}
	}
	if rooter, ok := e.Driver().(runtime.ProcessRooter); ok {
		if root, err := rooter.RootPID(ctx, id); err == nil && root > 0 {
			s.procs, s.procsRead, s.procsKnown = sampleProcesses(e.startStall.proc.orHost(), root)
		}
	}
	return s
}

// otherInstanceInFlight reports that another instance of this node is being
// started or stopped: this one may be waiting for something that one holds.
func (e *Engine) otherInstanceInFlight(uuid string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for other, busy := range e.inFlight {
		if busy && other != uuid {
			return true
		}
	}
	return false
}

// sampleCards reads memory in use and utilization of the instance's cards.
// It is known only when every card answers both, itself: a unified-memory
// card's memory is the host's pool, which every process on the host moves.
// The figures are the whole card's: on a card shared with another instance
// that one's activity reads as movement, and no stall is declared.
func (e *Engine) sampleCards(ctx context.Context, indexes []int) cardSample {
	ctx, cancel := context.WithTimeout(ctx, stallSampleTimeout)
	defer cancel()
	metrics, err := e.prober.Metrics(ctx)
	if err != nil {
		return cardSample{}
	}
	wanted := map[int]bool{}
	for _, index := range indexes {
		wanted[index] = true
	}
	var out cardSample
	seen := 0
	for _, m := range metrics {
		if !wanted[m.Index] {
			continue
		}
		used, hasUsed := m.Values["gpuworks_gpu_memory_used_bytes"]
		busy, hasBusy := m.Values["gpuworks_gpu_utilization"]
		if !hasUsed || !hasBusy || m.HostPoolMemory {
			return cardSample{}
		}
		seen++
		out.memory += used
		if busy > out.util {
			out.util = busy
		}
	}
	out.known = seen == len(wanted)
	return out
}

// sampleProcesses reads the processes that descend from root or share its
// process group. read is false when root is gone or /proc cannot be listed.
// known is false as well when a counter the watch compares could not be read
// for one of them: who traces it, or its bytes read and written (which an
// agent that is not root may not read of a container's processes, and a
// kernel without task I/O accounting does not keep). The states are still
// reported then; the counters are not compared.
func sampleProcesses(fs procFS, root int) (sample processSample, read, known bool) {
	entries, err := fs.readDir(fs.root)
	if err != nil {
		return processSample{}, false, false
	}
	type proc struct {
		processState
		ppid, pgrp, tracerPID int
		cpu, faults, io       uint64
	}
	all := map[int]*proc{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := fs.readFile(filepath.Join(fs.root, entry.Name(), "stat"))
		if err != nil {
			continue // exited between the listing and the read
		}
		p, ok := parseProcStat(raw)
		if !ok {
			continue
		}
		p.pid = pid
		all[pid] = &proc{processState: p.processState, ppid: p.ppid, pgrp: p.pgrp, cpu: p.cpu, faults: p.faults}
	}
	rootProc, ok := all[root]
	if !ok {
		return processSample{}, false, false
	}
	member := map[int]bool{root: true}
	for pid, p := range all {
		if p.pgrp == rootProc.pgrp && rootProc.pgrp == root {
			member[pid] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for pid, p := range all {
			if !member[pid] && member[p.ppid] {
				member[pid], changed = true, true
			}
		}
	}
	pids := make([]int, 0, len(member))
	for pid := range member {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	known = true
	for _, pid := range pids {
		p := all[pid]
		dir := filepath.Join(fs.root, strconv.Itoa(pid))
		p.tracerPID = -1
		if raw, err := fs.readFile(filepath.Join(dir, "status")); err == nil {
			p.tracerPID = procField(raw, "TracerPid:")
		}
		read, write := -1, -1
		if raw, err := fs.readFile(filepath.Join(dir, "io")); err == nil {
			read, write = procField(raw, "rchar:"), procField(raw, "wchar:")
		}
		if p.tracerPID < 0 || read < 0 || write < 0 {
			known = false
			continue
		}
		p.io = uint64(read) + uint64(write)
	}
	for _, pid := range pids {
		if tracer := all[pid].tracerPID; tracer > 0 && member[tracer] {
			all[tracer].tracer = true
		}
	}
	names := make([]string, 0, len(pids))
	for _, pid := range pids {
		p := all[pid]
		names = append(names, strconv.Itoa(pid))
		sample.states = append(sample.states, p.processState)
		if p.tracer {
			sample.tracerTicks += p.cpu
			continue
		}
		if p.tracerPID > 0 && member[p.tracerPID] {
			sample.traced++
		}
		sample.cpuTicks += p.cpu
		sample.faults += p.faults
		sample.ioBytes += p.io
		sample.blocked = sample.blocked || p.state == 'D'
	}
	sample.pids = strings.Join(names, ",")
	return sample, true, known
}

type procStat struct {
	processState
	ppid, pgrp  int
	cpu, faults uint64
}

// parseProcStat reads one /proc/<pid>/stat line. The command name is in
// parentheses and may itself hold spaces and parentheses, so the fields are
// counted from the last ")".
func parseProcStat(raw []byte) (procStat, bool) {
	open, end := bytes.IndexByte(raw, '('), bytes.LastIndexByte(raw, ')')
	if open < 0 || end < open {
		return procStat{}, false
	}
	fields := strings.Fields(string(raw[end+1:]))
	// state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime
	if len(fields) < 13 || len(fields[0]) != 1 {
		return procStat{}, false
	}
	ppid, err1 := strconv.Atoi(fields[1])
	pgrp, err2 := strconv.Atoi(fields[2])
	faults, err3 := strconv.ParseUint(fields[9], 10, 64)
	user, err4 := strconv.ParseUint(fields[11], 10, 64)
	system, err5 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		return procStat{}, false
	}
	return procStat{
		processState: processState{name: string(raw[open+1 : end]), state: fields[0][0]},
		ppid:         ppid, pgrp: pgrp, cpu: user + system, faults: faults,
	}, true
}

// procField is the integer after key in a "key: value" file, -1 without one.
func procField(raw []byte, key string) int {
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, key); ok {
			if value, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				return value
			}
		}
	}
	return -1
}

// startAborted reports that the engine's output says one of its processes
// failed and it is shutting down (enginelog.StartAborted), with the failure
// to report if the container is still there the next time it is asked.
func (e *Engine) startAborted(ctx context.Context, spec proto.InstanceDesiredState, id string) (proto.RuntimeFailure, string, bool) {
	if spec.LogDialect != enginelog.DialectSGLang {
		return proto.RuntimeFailure{}, "", false // no other engine says it; its output is not read for this
	}
	ctx, cancel := context.WithTimeout(ctx, stallSampleTimeout)
	defer cancel()
	rc, err := e.Driver().Logs(ctx, id, false, exitTailLines)
	if err != nil {
		return proto.RuntimeFailure{}, "", false
	}
	defer rc.Close()
	text, err := readEnd(rc, exitTailBytes)
	if err != nil {
		return proto.RuntimeFailure{}, "", false
	}
	cause, aborted := enginelog.StartAborted(spec.LogDialect, text)
	if !aborted {
		return proto.RuntimeFailure{}, "", false
	}
	out := exitOutput{causes: enginelog.Classify(text), root: commandlog.Redact(cause)}
	if out.root == "" {
		out.root = commandlog.Redact(enginelog.RootCause(text))
	}
	failure := proto.RuntimeFailure{Code: proto.ErrorEngineStartAborted, Phase: proto.PhaseHealthCheck}
	out.attach(&failure)
	return failure, out.message("engine reported a failed process during start and did not exit"), true
}

// startMakesNoProgress is the once-a-sample check of a newly created
// container that is still waiting for health: the engine said it gave up
// (seen twice in a row, so an engine that is exiting gets to exit and be
// reported as that), or the watch declares a stall. It returns the failure
// to report.
func (e *Engine) startMakesNoProgress(ctx context.Context, spec proto.InstanceDesiredState, id string, stall *stallWatch, abortedBefore *bool) (proto.RuntimeFailure, string, bool) {
	failure, message, aborted := e.startAborted(ctx, spec, id)
	if aborted && *abortedBefore {
		return failure, message, true
	}
	*abortedBefore = aborted
	if stall == nil {
		return proto.RuntimeFailure{}, "", false
	}
	sample := e.sampleStartStall(ctx, spec, id)
	if ctx.Err() != nil {
		return proto.RuntimeFailure{}, "", false
	}
	now := time.Now()
	if !stall.observe(now, sample, func() cardSample { return e.sampleCards(ctx, spec.GPUIndexes) }) || ctx.Err() != nil {
		return proto.RuntimeFailure{}, "", false
	}
	failure = proto.RuntimeFailure{Code: proto.ErrorEngineStartStalled, Phase: proto.PhaseHealthCheck, Detail: stall.detail(now)}
	e.attachOutput(ctx, id, &failure, true)
	return failure, fmt.Sprintf("engine start stalled: %s", failure.Detail), true
}
