package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

var stallStart = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// flatSample is a start that shows nothing: the same output and processes
// that used no CPU and read nothing. flatCards is its cards: idle, holding
// the same memory.
func flatSample() stallSample {
	return stallSample{
		outputKnown: true, output: "same",
		procsRead: true, procsKnown: true, procs: processSample{pids: "10,11", cpuTicks: 500, ioBytes: 1 << 20, faults: 7},
	}
}

func flatCards() cardSample { return cardSample{memory: 8 << 30, known: true} }

// feed observes one sample a minute and reports the minute a stall was
// declared at, or -1.
func feed(w *stallWatch, minutes int, at func(minute int) (stallSample, cardSample)) int {
	for minute := 0; minute <= minutes; minute++ {
		s, cards := at(minute)
		if w.observe(stallStart.Add(time.Duration(minute)*time.Minute), s, func() cardSample { return cards }) {
			return minute
		}
	}
	return -1
}

func testWatch() *stallWatch { return startStall{}.watch() }

// GWKS-420's premise, taken literally: nothing new in the output, nothing
// moving on the cards, the processes idle. The start is declared stalled
// once all of it has lasted the window, not before. The cards are first
// asked a minute in, when the rest is seen flat, so the window ends a minute
// after the twentieth.
func TestStartIsStalledOnceEverySignalHasBeenFlatForTheWindow(t *testing.T) {
	w := testWatch()
	if got := feed(w, 60, func(int) (stallSample, cardSample) { return flatSample(), flatCards() }); got != 21 {
		t.Fatalf("stall declared at minute %d, want 21", got)
	}
	detail := w.detail(stallStart.Add(21 * time.Minute))
	for _, want := range []string{"output flat 21m0s", "cards flat 20m0s", "processes flat 21m0s"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q lacks %q", detail, want)
		}
	}
}

// Each signal moving on its own is progress: the quiet phases of a valid
// start each move one of them.
func TestOneMovingSignalIsProgress(t *testing.T) {
	for name, move := range map[string]func(minute int, s *stallSample, c *cardSample){
		"new output":                        func(m int, s *stallSample, _ *cardSample) { s.output = fmt.Sprint("line ", m/15) },
		"card memory grows while loading":   func(m int, _ *stallSample, c *cardSample) { c.memory += float64(m) * (128 << 20) },
		"card busy while capturing a graph": func(m int, _ *stallSample, c *cardSample) { c.util = 40 },
		"CPU while compiling":               func(m int, s *stallSample, _ *cardSample) { s.procs.cpuTicks += uint64(m) * 60 * stallClockTicks },
		"bytes read from slow storage":      func(m int, s *stallSample, _ *cardSample) { s.procs.ioBytes += uint64(m) * 60 * (1 << 20) },
		"pages faulted in from a mapping":   func(m int, s *stallSample, _ *cardSample) { s.procs.faults += uint64(m) * 600 },
		"a process in disk wait":            func(m int, s *stallSample, _ *cardSample) { s.procs.blocked = true },
		"a worker was spawned":              func(m int, s *stallSample, _ *cardSample) { s.procs.pids = fmt.Sprint("10,11,", 12+m/15) },
	} {
		t.Run(name, func(t *testing.T) {
			got := feed(testWatch(), 120, func(minute int) (stallSample, cardSample) {
				s, c := flatSample(), flatCards()
				move(minute, &s, &c)
				return s, c
			})
			if got != -1 {
				t.Fatalf("a start with %s was declared stalled at minute %d", name, got)
			}
		})
	}
}

// What the agent cannot observe is unknown, and an unknown signal keeps a
// stall from being declared however flat the others are. Once it is known
// again its own window starts over. Process counters that cannot be read
// (procsRead without procsKnown: an agent that may not read a container's
// I/O counters) are unknown like the rest.
func TestAnUnknownSignalIsNeverFlat(t *testing.T) {
	for name, unknown := range map[string]func(s *stallSample, c *cardSample){
		"output":               func(s *stallSample, _ *cardSample) { s.outputKnown = false },
		"cards":                func(_ *stallSample, c *cardSample) { c.known = false },
		"processes":            func(s *stallSample, _ *cardSample) { s.procsRead, s.procsKnown = false, false },
		"process counters":     func(s *stallSample, _ *cardSample) { s.procsKnown = false },
		"a peer being started": func(s *stallSample, _ *cardSample) { s.peerBusy = true },
	} {
		t.Run(name, func(t *testing.T) {
			got := feed(testWatch(), 120, func(int) (stallSample, cardSample) {
				s, c := flatSample(), flatCards()
				unknown(&s, &c)
				return s, c
			})
			if got != -1 {
				t.Fatalf("declared stalled at minute %d with %s unknown", got, name)
			}
			got = feed(testWatch(), 120, func(minute int) (stallSample, cardSample) {
				s, c := flatSample(), flatCards()
				if minute < 50 {
					unknown(&s, &c)
				}
				return s, c
			})
			if got < 70 || got > 71 {
				t.Fatalf("declared at minute %d, want a full window after %s was first known (70 or 71)", got, name)
			}
		})
	}
}

// Noise is not progress: a card whose memory wobbles by a few megabytes and
// a process tree that wakes for a moment leave the start stalled.
func TestNoiseIsNotProgress(t *testing.T) {
	got := feed(testWatch(), 60, func(minute int) (stallSample, cardSample) {
		s, c := flatSample(), flatCards()
		c.memory += float64(minute%2) * (4 << 20)
		c.util = 1
		s.procs.cpuTicks += uint64(minute) * 2 * stallClockTicks // two CPU seconds a minute
		s.procs.ioBytes += uint64(minute) * 1024
		return s, c
	})
	if got != 21 {
		t.Fatalf("stall declared at minute %d, want 21", got)
	}
}

// A verdict needs samples. Two looks 25 minutes apart say nothing about the
// time between them, however flat both are, and a gap longer than three
// intervals starts the window over.
func TestAVerdictNeedsSamplesThroughoutTheWindow(t *testing.T) {
	at := func(w *stallWatch, minute int) bool {
		return w.observe(stallStart.Add(time.Duration(minute)*time.Minute), flatSample(), flatCards)
	}
	w := testWatch()
	if at(w, 0) || at(w, 25) || at(w, 50) {
		t.Fatal("declared from looks 25 minutes apart")
	}
	// Samples every three minutes are within the gap but too few: seven in
	// twenty minutes, where ten flat ones are asked for. The first compares
	// with nothing and the second is the cards' first reading, so the tenth
	// flat sample is the one at minute 33.
	w = testWatch()
	declared := -1
	for minute := 0; minute <= 60 && declared < 0; minute += 3 {
		if at(w, minute) {
			declared = minute
		}
	}
	if declared != 33 {
		t.Fatalf("sparse samples: declared at minute %d, want 33 (the tenth flat sample)", declared)
	}
	// A gap in an otherwise steady watch: 18 flat minutes, nothing for 5,
	// then flat again. The window starts after the gap.
	w = testWatch()
	declared = -1
	for minute := 0; minute <= 80 && declared < 0; minute++ {
		if minute > 18 && minute < 23 {
			continue
		}
		if at(w, minute) {
			declared = minute
		}
	}
	if declared != 44 {
		t.Fatalf("after a gap: declared at minute %d, want 44 (a window after minute 23)", declared)
	}
}

// The cards are asked only when the output and the processes are seen flat:
// a start that shows progress elsewhere costs no card query.
func TestCardsAreAskedOnlyWhenTheRestIsFlat(t *testing.T) {
	asked := 0
	cards := func() cardSample { asked++; return flatCards() }
	w := testWatch()
	for minute := 0; minute <= 30; minute++ {
		s := flatSample()
		s.output = fmt.Sprint(minute) // a line a minute
		if w.observe(stallStart.Add(time.Duration(minute)*time.Minute), s, cards) {
			t.Fatalf("declared at minute %d", minute)
		}
	}
	if asked != 0 {
		t.Fatalf("the cards were asked %d times during a start that printed a line a minute", asked)
	}
	for minute := 31; minute <= 40; minute++ {
		s := flatSample()
		s.output = "30"
		w.observe(stallStart.Add(time.Duration(minute)*time.Minute), s, cards)
	}
	if asked != 10 {
		t.Fatalf("the cards were asked %d times in ten flat minutes, want 10", asked)
	}
}

// A long valid start, modelled on the startup reference (checkpoint read,
// then initialization, then graph capture with nothing printed for half an
// hour): no stall, although the output is silent far longer than the window.
func TestALongValidStartIsNotStalled(t *testing.T) {
	got := feed(testWatch(), 110, func(minute int) (stallSample, cardSample) {
		s, c := flatSample(), flatCards()
		switch {
		case minute < 40: // weights from slow storage: bytes read, memory grows
			s.output = fmt.Sprint("loading shard ", minute/10)
			s.procs.ioBytes += uint64(minute) * (512 << 20)
			c.memory = float64(minute) * (512 << 20)
		case minute < 75: // graph capture: silent, cards busy
			s.output, c.memory, c.util = "loading shard 3", 20<<30, 60
		default: // CPU-only warm-up: silent, cards idle
			s.output, c.memory = "loading shard 3", 20<<30
			s.procs.cpuTicks += uint64(minute) * 50 * stallClockTicks
		}
		return s, c
	})
	if got != -1 {
		t.Fatalf("a valid start was declared stalled at minute %d", got)
	}
}

// writeProc writes one process of a /proc fixture.
func writeProc(t *testing.T, root string, pid int, comm string, state byte, ppid, pgrp, tracer int, cpu, faults, io uint64) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// pid (comm) state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime ...
	stat := fmt.Sprintf("%d (%s) %c %d %d %d 0 -1 4194304 74 0 %d 0 %d %d 0 0 20 0 1 0 100 1000 10\n", pid, comm, state, ppid, pgrp, pgrp, faults, cpu-cpu/2, cpu/2)
	status := fmt.Sprintf("Name:\t%s\nState:\t%c\nTracerPid:\t%d\n", comm, state, tracer)
	files := map[string]string{"stat": stat, "status": status}
	if io > 0 {
		files["io"] = fmt.Sprintf("rchar: %d\nwchar: 0\nsyscr: 1\n", io)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The process tree of GWKS-420 as /proc shows it: a PRoot shim that spins,
// two workers it traces in tracing stop, two that sleep, and an unrelated
// process of the host that is busy.
func stalledProcFixture(t *testing.T, tracerCPU uint64) string {
	t.Helper()
	root := t.TempDir()
	writeProc(t, root, 100, "proot-arm64", 'R', 1, 100, 0, tracerCPU, 0, 1)
	writeProc(t, root, 101, "python3 (vllm) serve", 'S', 100, 100, 100, 4000, 20, 5<<20)
	writeProc(t, root, 110, "VLLM::Worker_TP0", 't', 101, 100, 100, 300, 9, 1<<20)
	writeProc(t, root, 111, "VLLM::Worker_TP1", 't', 101, 100, 100, 300, 9, 1<<20)
	writeProc(t, root, 112, "VLLM::Worker_TP2", 'S', 101, 112, 100, 9000, 40, 9<<30)
	writeProc(t, root, 113, "VLLM::Worker_TP3", 'S', 101, 113, 100, 9000, 40, 9<<30)
	writeProc(t, root, 900, "busy-elsewhere", 'R', 1, 900, 0, 1<<30, 1<<20, 1<<40)
	for _, name := range []string{"self", "sys", "meminfo"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestProcessSampleLeavesOutTheTracerAndForeignProcesses(t *testing.T) {
	root := stalledProcFixture(t, 5000)
	got, read, known := sampleProcesses(procFS{root: root}.orHost(), 100)
	if !read || !known {
		t.Fatalf("the container's processes: read=%v known=%v", read, known)
	}
	if got.pids != "100,101,110,111,112,113" {
		t.Fatalf("processes = %s, want the group leader and its descendants only", got.pids)
	}
	if want := uint64(4000 + 300 + 300 + 9000 + 9000); got.cpuTicks != want {
		t.Fatalf("CPU ticks = %d, want %d: the tracer's own time must not count", got.cpuTicks, want)
	}
	if got.faults != 20+9+9+40+40 || got.blocked || got.traced != 5 || got.tracerTicks != 5000 {
		t.Fatalf("sample = %+v", got)
	}
	want := "processes: 2 in tracing stop (VLLM::Worker_TP0, VLLM::Worker_TP1), 3 sleeping; tracer proot-arm64 running"
	if summary := got.summary(); summary != want {
		t.Fatalf("summary = %q\nwant      %q", summary, want)
	}

	// The tracer burns a minute of CPU while its tracees stand still: flat.
	later, _, _ := sampleProcesses(procFS{root: stalledProcFixture(t, 5000+60*stallClockTicks)}.orHost(), 100)
	if later.movedSince(got, time.Minute) {
		t.Fatal("a spinning tracer was taken for progress of the engine")
	}
	if share := later.tracerShareSince(got, time.Minute); share < 0.99 || share > 1.01 {
		t.Fatalf("tracer share = %.2f, want one core", share)
	}
	if _, read, _ := sampleProcesses(procFS{root: root}.orHost(), 4242); read {
		t.Fatal("a root process that is gone was sampled")
	}
	if _, read, _ := sampleProcesses(procFS{root: filepath.Join(root, "absent")}.orHost(), 100); read {
		t.Fatal("an unreadable /proc was sampled")
	}
}

// What an agent that is not root sees of a root container's processes, and
// any agent on a kernel without task I/O accounting: stat readable, io not.
// The reads are injected, so this does not depend on who runs the test. The
// states are still reported; the counters are unknown, and unknown counters
// never let a stall be declared.
func TestUnreadableProcessCountersAreUnknownNotFlat(t *testing.T) {
	root := stalledProcFixture(t, 5000)
	for name, denied := range map[string]string{
		"io of one process":              filepath.Join("110", "io"),
		"io of every process":            "io",
		"status of one process (tracer)": filepath.Join("111", "status"),
	} {
		t.Run(name, func(t *testing.T) {
			fs := procFS{root: root, readFile: func(path string) ([]byte, error) {
				if strings.HasSuffix(path, denied) {
					return nil, os.ErrPermission
				}
				return os.ReadFile(path)
			}}.orHost()
			got, read, known := sampleProcesses(fs, 100)
			if !read || known {
				t.Fatalf("read=%v known=%v, want the states read and the counters unknown", read, known)
			}
			if !strings.Contains(got.summary(), "2 in tracing stop") {
				t.Fatalf("summary = %q", got.summary())
			}
			// Through the watch: flat in every other respect for two hours.
			declared := feed(testWatch(), 120, func(int) (stallSample, cardSample) {
				s := flatSample()
				s.procs, s.procsRead, s.procsKnown = got, read, known
				return s, flatCards()
			})
			if declared != -1 {
				t.Fatalf("declared stalled at minute %d with unreadable counters", declared)
			}
		})
	}
	// A stat that cannot be read is a process that is not there: the tree is
	// what remains, and a root whose stat cannot be read is no tree at all.
	fs := procFS{root: root, readFile: func(path string) ([]byte, error) {
		if strings.HasSuffix(path, filepath.Join("100", "stat")) {
			return nil, os.ErrPermission
		}
		return os.ReadFile(path)
	}}.orHost()
	if _, read, _ := sampleProcesses(fs, 100); read {
		t.Fatal("a tree whose root cannot be read was sampled")
	}
	listing := procFS{root: root, readDir: func(string) ([]os.DirEntry, error) { return nil, os.ErrPermission }}.orHost()
	if _, read, _ := sampleProcesses(listing, 100); read {
		t.Fatal("a /proc that cannot be listed was sampled")
	}
}

// stallDriver is the liveness fake with the two things the watch asks of a
// runtime: the output's end and the container's root process.
type stallDriver struct {
	*livenessDriver
	root    int
	rootErr error
}

func (d *stallDriver) RootPID(context.Context, string) (int, error) { return d.root, d.rootErr }

type stallProber struct {
	injectionProber
	metrics []gpu.Metric
	err     error
}

func (p stallProber) Metrics(context.Context) ([]gpu.Metric, error) { return p.metrics, p.err }

func idleCards(indexes ...int) []gpu.Metric {
	out := make([]gpu.Metric, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, gpu.Metric{Index: index, Values: map[string]float64{"gpuworks_gpu_memory_used_bytes": 3 << 30, "gpuworks_gpu_utilization": 0}})
	}
	return out
}

// stalledEngine is an engine whose instance answers 503 forever, with the
// stalled process tree and idle cards, and a window short enough to pass
// between two health checks.
func stalledEngine(t *testing.T, driver runtime.Driver, prober gpu.Prober) (*Engine, *statusLog, proto.InstanceDesiredState, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	log := &statusLog{}
	e := NewWithProber(driver, log.report, slog.New(slog.DiscardHandler), t.TempDir(), prober)
	e.startStall = startStall{window: time.Millisecond, interval: time.Millisecond, minSamples: 2, maxGap: time.Minute, proc: procFS{root: stalledProcFixture(t, 5000)}}
	t.Cleanup(e.stopHealthProbes)
	spec := runningSpec("stalled-start", serverPort(t, server))
	spec.GPUIndexes = []int{2, 3}
	// Far beyond the test: only the stall can end this start.
	spec.StartTimeout = 3600
	name := containerName(spec.UUID)
	if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
		t.Fatal(err)
	}
	return e, log, spec, name
}

// The whole path: an engine that hangs is failed as stalled long before its
// start timeout, with its own code, what its processes are doing and what it
// printed last.
func TestAStalledStartIsFailedBeforeItsTimeoutWithWhatItWasDoing(t *testing.T) {
	driver := &stallDriver{livenessDriver: newLivenessDriver(), root: 100}
	driver.logs = hungStartOutput
	e, log, spec, name := stalledEngine(t, driver, stallProber{metrics: idleCards(0, 1, 2, 3)})
	began := time.Now()
	// Without the watch this start would wait an hour; the test does not.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if e.probeAndReportStartup(ctx, spec, name, true, true) {
		t.Fatal("a stalled engine was declared ready")
	}
	if took := time.Since(began); took > time.Minute {
		t.Fatalf("the stall was declared after %s", took)
	}
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || failed[0].RuntimeFailure == nil {
		t.Fatalf("reports = %+v", log.snapshot())
	}
	f := failed[0].RuntimeFailure
	if f.Code != proto.ErrorEngineStartStalled || f.Phase != proto.PhaseHealthCheck || len(f.Causes) != 0 {
		t.Fatalf("failure = %+v", f)
	}
	for _, want := range []string{"no progress: output flat ", "cards flat ", "processes flat ", "2 in tracing stop (VLLM::Worker_TP0, VLLM::Worker_TP1)", "tracer proot-arm64 running"} {
		if !strings.Contains(f.Detail, want) {
			t.Fatalf("detail %q lacks %q", f.Detail, want)
		}
	}
	if !slices.Equal(f.Evidence, hungStartEvidence) {
		t.Fatalf("evidence = %q, want %q", f.Evidence, hungStartEvidence)
	}
	if message := failed[0].Message; !strings.HasPrefix(message, "engine start stalled: no progress: ") || strings.Contains(message, "SECRET") {
		t.Fatalf("message = %q", message)
	}
	if failed[0].Measured != nil {
		t.Fatalf("a stalled start is recorded as a start whose budget ran out: %s", failed[0].Measured)
	}

	// On the wire, as the server stores it and the console reads it.
	bounded := f.Bounded()
	bounded.Detail = "D"
	raw, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"code":"ENGINE_START_STALLED","phase":"health_check","detail":"D","evidence":["No module named 'cutlass'","Loading with HF_TOKEN=\u003credacted\u003e from the launch script","[Gloo] Rank 0 is connected to 0 peer ranks. Expected number of connected peer ranks is : 0"]}`
	if string(raw) != want {
		t.Fatalf("wire = %s\nwant   %s", raw, want)
	}
	if len(f.Bounded().Detail) > proto.MaxFailureEvidenceChars {
		t.Fatalf("detail is %d bytes", len(f.Detail))
	}
}

// Where a signal cannot be observed, or the instance is one the watch leaves
// alone, the same hung engine waits for its start timeout as before.
func TestAStartIsNotWatchedWhereItsSignalsCannotBeObserved(t *testing.T) {
	type setup struct {
		driver *stallDriver
		prober stallProber
		// used is the driver the engine runs on; spec changes the instance.
		used runtime.Driver
		spec func(*proto.InstanceDesiredState)
		// denyIO: /proc/<pid>/io cannot be read. peer: another instance is
		// in flight. adopted: the wait is for a container found running.
		denyIO, peer, adopted bool
	}
	for name, change := range map[string]func(*setup){
		"the runtime cannot name the container's processes": func(s *setup) { s.used = s.driver.livenessDriver },
		"the container's root process is unknown":           func(s *setup) { s.driver.rootErr = errors.New("no recorded process") },
		"the container's root process is gone from /proc":   func(s *setup) { s.driver.root = 4242 },
		"the card metrics do not answer":                    func(s *setup) { s.prober.err = errors.New("npu-smi did not answer") },
		"one of the instance's cards is missing":            func(s *setup) { s.prober.metrics = idleCards(0, 1, 2) },
		"a card reports no utilization": func(s *setup) {
			delete(s.prober.metrics[2].Values, "gpuworks_gpu_utilization")
		},
		"a card's memory is the host's pool": func(s *setup) { s.prober.metrics[3].HostPoolMemory = true },
		"a card is busy":                     func(s *setup) { s.prober.metrics[2].Values["gpuworks_gpu_utilization"] = 80 },
		"the instance has no card": func(s *setup) {
			s.spec = func(spec *proto.InstanceDesiredState) { spec.GPUIndexes = nil }
		},
		"the instance is a rank of a multi-node unit": func(s *setup) {
			s.spec = func(spec *proto.InstanceDesiredState) { spec.NNodes = 2 }
		},
		"the process counters cannot be read":           func(s *setup) { s.denyIO = true },
		"another instance of the node is being started": func(s *setup) { s.peer = true },
		"the container is adopted, not started":         func(s *setup) { s.adopted = true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := &setup{driver: &stallDriver{livenessDriver: newLivenessDriver(), root: 100}, prober: stallProber{metrics: idleCards(0, 1, 2, 3)}}
			s.driver.logs = hungStartOutput
			s.used = s.driver
			change(s)
			e, log, spec, name := stalledEngine(t, s.used, s.prober)
			if s.spec != nil {
				s.spec(&spec)
			}
			// Long enough for the four samples a stall is declared from here
			// (TestAStalledStartIsFailed...), so a watched start would show.
			spec.StartTimeout = 12
			if s.denyIO {
				e.startStall.proc.readFile = func(path string) ([]byte, error) {
					if strings.HasSuffix(path, "io") {
						return nil, os.ErrPermission
					}
					return os.ReadFile(path)
				}
			}
			if s.peer {
				e.mu.Lock()
				e.inFlight["a-peer"] = true
				e.mu.Unlock()
			}
			if e.probeAndReportStartup(t.Context(), spec, name, !s.adopted, true) {
				t.Fatal("an engine answering 503 was declared ready")
			}
			failed := log.states(proto.InstFailed)
			if len(failed) != 1 || failed[0].RuntimeFailure == nil || failed[0].RuntimeFailure.Code != proto.ErrorEngineHealthTimeout {
				t.Fatalf("reports = %+v", log.snapshot())
			}
		})
	}
}
