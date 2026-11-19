package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// The tests here use what was captured on real nodes (GWKS-354, GWKS-373,
// GWKS-420; pkg/enginelog/testdata/field). Captured: the engines' output with
// its timestamps, and two readings of /proc 22 seconds apart during one
// starved start. Everything else a replay needs is a model and is named as
// one where it is used: what the cards held (read off the log's own lines
// about weights), and what the processes did at any other moment.

func fieldLog(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "pkg", "enginelog", "testdata", "field", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var (
	fieldStampRE = regexp.MustCompile(`\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})|(?:INFO|WARNING|ERROR) (\d{2}-\d{2} \d{2}:\d{2}:\d{2}) `)
	// A progress line has no timestamp of its own but says how long the load
	// has run: "... 57% Completed | 8/14 [05:01<03:15, 32.64s/it]".
	fieldShardRE = regexp.MustCompile(`Loading safetensors checkpoint shards:.*\| (\d+)/\d+ \[(\d+):(\d+)<`)
)

// fieldLine is one line of a captured log with the time it was printed at; a
// line without a timestamp of its own has the one before it, and a shard
// progress line the load's start plus the elapsed time it states.
type fieldLine struct {
	at   time.Time
	text string
}

func fieldLines(t *testing.T, name string) []fieldLine {
	t.Helper()
	var out []fieldLine
	var at, loadStart time.Time
	for _, line := range strings.Split(strings.TrimRight(fieldLog(t, name), "\n"), "\n") {
		lineAt := at
		if m := fieldStampRE.FindStringSubmatch(line); m != nil {
			stamp := m[1]
			if stamp == "" {
				stamp = "2026-" + m[2]
			}
			parsed, err := time.Parse("2006-01-02 15:04:05", stamp)
			if err != nil {
				t.Fatalf("%s: %q: %v", name, line, err)
			}
			// A worker's line can carry a time a second before the line
			// above it; the log's own order is what was printed when.
			if parsed.After(at) {
				at = parsed
			}
			lineAt = at
		} else if m := fieldShardRE.FindStringSubmatch(line); m != nil && !at.IsZero() {
			if m[1] == "0" || loadStart.IsZero() {
				loadStart = at
			}
			var minutes, seconds int
			fmt.Sscan(m[2], &minutes)
			fmt.Sscan(m[3], &seconds)
			lineAt = loadStart.Add(time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second)
		}
		if lineAt.IsZero() {
			continue // before the engine's first timestamp
		}
		out = append(out, fieldLine{at: lineAt, text: line})
	}
	if len(out) < 20 {
		t.Fatalf("%s: %d timed lines", name, len(out))
	}
	return out
}

// The two readings of /proc taken 22 seconds apart during the starved SGLang
// start (16:38:35 and 16:38:57): the PRoot shim and the four schedulers it
// traces, with their state and CPU time as captured. Not captured, and filled
// in so that the readings can be parsed: the schedulers' parent (placed under
// the shim, whose group they are in) and their I/O counters (written as
// zero).
type starvedProc struct {
	pid          int
	comm         string
	state        byte
	tracer       int
	user, system [2]uint64
}

var starvedProcs = []starvedProc{
	{2587325, "proot-arm64", 'R', 0, [2]uint64{38973, 40180}, [2]uint64{39877, 40859}},
	{2600199, "sglang::schedul", 't', 2587325, [2]uint64{2485, 2487}, [2]uint64{5123, 5263}},
	{2600200, "sglang::schedul", 't', 2587325, [2]uint64{2482, 2487}, [2]uint64{4240, 4358}},
	{2600201, "sglang::schedul", 't', 2587325, [2]uint64{2424, 2425}, [2]uint64{5149, 5283}},
	{2600202, "sglang::schedul", 't', 2587325, [2]uint64{2465, 2471}, [2]uint64{5068, 5202}},
}

const starvedSampleGap = 22 * time.Second

// starvedReading parses reading 0 or 1.
func starvedReading(t *testing.T, reading int) processSample {
	t.Helper()
	root := t.TempDir()
	for _, p := range starvedProcs {
		ppid := 2587325
		if p.tracer == 0 {
			ppid = 1
		}
		dir := filepath.Join(root, fmt.Sprint(p.pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (%s) %c %d 2587325 2587325 0 -1 4194304 74 0 0 0 %d %d 0 0 20 0 1 0 100 1000 10\n", p.pid, p.comm, p.state, ppid, p.user[reading], p.system[reading])
		status := fmt.Sprintf("Name:\t%s\nState:\t%c\nTracerPid:\t%d\n", p.comm, p.state, p.tracer)
		for name, body := range map[string]string{"stat": stat, "status": status, "io": "rchar: 0\nwchar: 0\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	sample, read, known := sampleProcesses(procFS{root: root}.orHost(), 2587325)
	if !read || !known {
		t.Fatalf("reading %d: read=%v known=%v", reading, read, known)
	}
	return sample
}

// What the two readings say, and all they say: over those 22 seconds one
// tracer used a whole core, the four processes it traces were in tracing
// stop at both instants, and together they still used a quarter of a core.
// That is progress by the watch's own threshold (a fifth of a core), so this
// start was not hung at that moment. Whether a busy valid start under the
// same tracer looks any different was not measured, which is why no stall is
// declared from this picture; it is reported with the start timeout instead.
func TestTheTwoCapturedProcessReadingsShowASaturatedTracerOverProcessesThatAdvance(t *testing.T) {
	first, second := starvedReading(t, 0), starvedReading(t, 1)
	if second.traced != 4 {
		t.Fatalf("traced = %d, want 4", second.traced)
	}
	if share := second.tracerShareSince(first, starvedSampleGap); share < 0.95 || share > 1.05 {
		t.Fatalf("tracer share = %.2f of a core, want about one", share)
	}
	tracees := float64(second.cpuTicks-first.cpuTicks) / (starvedSampleGap.Seconds() * stallClockTicks)
	if tracees < 0.22 || tracees > 0.27 {
		t.Fatalf("the traced processes used %.3f of a core, want about a quarter", tracees)
	}
	if !second.movedSince(first, starvedSampleGap) {
		t.Fatal("the traced processes read as idle")
	}
	want := "processes: 4 in tracing stop (sglang::schedul, sglang::schedul, sglang::schedul); tracer proot-arm64 running"
	if got := second.summary(); got != want {
		t.Fatalf("summary = %q\nwant      %q", got, want)
	}
}

// replay feeds a watch one sample a minute over a captured log played at
// stretch times its real duration (1 is the log as captured; more is a
// synthetic slower start with the same phases). The output is what had been
// printed by then; cards and processes are the caller's model. It returns
// when a stall was declared, as time since the log's first line, or -1.
func replay(t *testing.T, lines []fieldLine, stretch int, cards func(printed []fieldLine) cardSample, processes func(minute int, quiet time.Duration) processSample) time.Duration {
	t.Helper()
	w := testWatch()
	start := lines[0].at
	total := time.Duration(stretch) * lines[len(lines)-1].at.Sub(start)
	printed := 0
	var lastPrinted time.Duration
	for minute := 0; ; minute++ {
		elapsed := time.Duration(minute) * time.Minute
		if elapsed > total {
			return -1
		}
		for printed < len(lines) && time.Duration(stretch)*lines[printed].at.Sub(start) <= elapsed {
			lastPrinted = time.Duration(stretch) * lines[printed].at.Sub(start)
			printed++
		}
		s := stallSample{outputKnown: true, output: fmt.Sprint(printed), procsRead: true, procsKnown: true, procs: processes(minute, elapsed-lastPrinted)}
		if w.observe(stallStart.Add(elapsed), s, func() cardSample { return cards(lines[:printed]) }) {
			return elapsed
		}
	}
}

// cardsFromLog is a model, not a capture: the cards hold what the log says
// was put on them (a shard, a worker's weights) and are otherwise idle.
func cardsFromLog(printed []fieldLine) cardSample {
	out := cardSample{memory: 3 << 30, known: true}
	shards := 0
	for _, line := range printed {
		if m := fieldShardRE.FindStringSubmatch(line.text); m != nil {
			fmt.Sscan(m[1], &shards)
		}
		if strings.Contains(line.text, "Loading model weights took") || strings.Contains(line.text, "Load weight end") {
			out.memory += 8 << 30
		}
	}
	out.memory += float64(shards) * (2 << 30)
	return out
}

// The model must see the weights arrive in each captured valid start, or the
// replays below would prove nothing about cards.
func TestTheCardModelSeesWeightsArriveInEveryCapturedValidStart(t *testing.T) {
	for _, log := range []string{"vllm_ascend_udocker_tp2_healthy.txt", "vllm_hygon_docker_healthy.txt", "sglang_ascend_nerdctl_healthy.txt"} {
		lines := fieldLines(t, log)
		idle := cardsFromLog(lines[:1]).memory
		steps := 0
		last := idle
		for i := range lines {
			if now := cardsFromLog(lines[:i+1]).memory; now != last {
				steps, last = steps+1, now
			}
		}
		if steps == 0 || last <= idle {
			t.Errorf("%s: the modelled cards never move", log)
		}
		if log == "vllm_hygon_docker_healthy.txt" && steps < 13 {
			t.Errorf("%s: %d steps, want one per shard", log, steps)
		}
	}
}

// The captured starts of GWKS-420 are not declared stalled, and that is the
// limit of this watch: they advanced. Output is the captured log. Processes
// are the two captured readings carried forward at their rate for the whole
// start, which is an extrapolation. Cards are the model.
//
// If instead their processes had been idle whenever the log was silent,
// which the readings contradict for the one moment they cover, the vLLM
// start would have been declared in the 33 minutes of silence after its
// second worker loaded; the SGLang one never, since it printed a line every
// few minutes.
func TestStartsThatAdvanceSlowlyAreLeftToTheStartTimeout(t *testing.T) {
	first, second := starvedReading(t, 0), starvedReading(t, 1)
	advancing := func(minute int, _ time.Duration) processSample {
		grown := func(from, to uint64) uint64 {
			return from + uint64(float64(to-from)*float64(minute)*float64(time.Minute)/float64(starvedSampleGap))
		}
		out := second
		out.cpuTicks, out.tracerTicks = grown(first.cpuTicks, second.cpuTicks), grown(first.tracerTicks, second.tracerTicks)
		return out
	}
	idleWhenSilent := func(int, time.Duration) processSample { return second }
	for log, wantIfIdle := range map[string]time.Duration{
		"sglang_ascend_udocker_tp4_starved.txt": -1,
		"vllm_ascend_udocker_tp4_starved.txt":   30 * time.Minute, // 12:47:32, a window after 12:26:13
	} {
		lines := fieldLines(t, log)
		if at := replay(t, lines, 1, cardsFromLog, advancing); at != -1 {
			t.Errorf("%s with advancing processes: declared %s in", log, at)
		}
		if at := replay(t, lines, 1, cardsFromLog, idleWhenSilent); at != wantIfIdle {
			t.Errorf("%s with idle processes: declared %s in, want %s", log, at, wantIfIdle)
		}
	}
}

// The captured valid starts are never declared, under the assumption worst
// for them: processes that do nothing at all, so that only the output and
// the modelled cards speak. At their real speed no silence is as long as the
// window. Played four times slower (a synthetic start with the same phases)
// their silences are, and then what keeps them alive is the work their
// processes do in those silences: a compile that prints nothing for an hour
// is not a stall while it uses a core. With idle processes the slowed Hygon
// start is declared, in its graph compile, which is the watch working as
// designed on a start that really does nothing.
func TestCapturedValidStartsAreNotStalled(t *testing.T) {
	idle := func(int, time.Duration) processSample { return flatSample().procs }
	working := func(minute int, quiet time.Duration) processSample {
		p := flatSample().procs
		p.cpuTicks += uint64(minute) * 60 * stallClockTicks // one core throughout
		return p
	}
	for _, log := range []string{
		"vllm_ascend_udocker_tp2_healthy.txt", // udocker, the node of GWKS-420: 7.6 minutes
		"vllm_hygon_docker_healthy.txt",       // 17.5 minutes: shards for 7, then a 215 s graph compile
		"sglang_ascend_nerdctl_healthy.txt",
	} {
		lines := fieldLines(t, log)
		if at := replay(t, lines, 1, cardsFromLog, idle); at != -1 {
			t.Errorf("%s at its real speed, idle processes: declared %s in", log, at)
		}
		for _, stretch := range []int{4, 8} {
			if at := replay(t, lines, stretch, cardsFromLog, working); at != -1 {
				t.Errorf("%s %d times slower, working processes: declared %s in", log, stretch, at)
			}
		}
	}
	// The bite: eight times slower, the Hygon start's compile is 29 silent
	// minutes with its weights already loaded. Idle processes then are a
	// stall; this is what the "working" replays above are not.
	if at := replay(t, fieldLines(t, "vllm_hygon_docker_healthy.txt"), 8, cardsFromLog, idle); at == -1 {
		t.Error("the slowed Hygon start with idle processes was not declared: the replays above cannot fail")
	}
}

// A busy start under a saturated tracer is not a stall, whatever its cards
// do: four cores of work in the traced processes, a new line every minute, a
// gigabyte read every minute, the tracer at 92 % of a core, one traced
// process caught in tracing stop, the cards flat for two hours. (A rule that
// judged the tracer and the cards alone declared this start; the review of
// that rule built this case.)
func TestABusyStartUnderASaturatedTracerIsNotStalled(t *testing.T) {
	got := feed(testWatch(), 120, func(minute int) (stallSample, cardSample) {
		s := flatSample()
		s.output = fmt.Sprint(minute)
		s.procs.cpuTicks += uint64(minute) * 4 * 60 * stallClockTicks
		s.procs.ioBytes += uint64(minute) << 30
		s.procs.tracerTicks = uint64(minute) * 55 * stallClockTicks
		s.procs.traced = 6
		s.procs.states = []processState{{name: "proot", state: 'R', tracer: true}, {name: "worker", state: 't'}}
		return s, flatCards()
	})
	if got != -1 {
		t.Fatalf("declared at minute %d", got)
	}
	// Nor with only one of those signs of work, the cards still flat.
	for name, work := range map[string]func(minute int, s *stallSample){
		"CPU only":    func(m int, s *stallSample) { s.procs.cpuTicks += uint64(m) * 60 * stallClockTicks },
		"reads only":  func(m int, s *stallSample) { s.procs.ioBytes += uint64(m) << 30 },
		"output only": func(m int, s *stallSample) { s.output = fmt.Sprint(m / 10) },
	} {
		if got := feed(testWatch(), 120, func(minute int) (stallSample, cardSample) {
			s := flatSample()
			s.procs.tracerTicks, s.procs.traced = uint64(minute)*55*stallClockTicks, 6
			work(minute, &s)
			return s, flatCards()
		}); got != -1 {
			t.Errorf("%s: declared at minute %d", name, got)
		}
	}
}

// The start captured on nerdctl whose schedulers all raised 37 seconds after
// the server came up, while the server process stayed and answered its health
// check as not ready: it is failed on the second look at its output, with
// the exception the schedulers raised, not at its start timeout.
func TestAnEngineThatSaysItGaveUpIsFailedWithoutWaitingForItsTimeout(t *testing.T) {
	crashed := fieldLog(t, "sglang_ascend_nerdctl_schedulers_crashed.txt")
	for name, tc := range map[string]struct {
		logs, dialect string
		wantCode      string
	}{
		"the crashed start":                    {crashed, enginelog.DialectSGLang, proto.ErrorEngineStartAborted},
		"the same output from another engine":  {crashed, enginelog.DialectVLLM, proto.ErrorEngineHealthTimeout},
		"the healthy start of the same model":  {fieldLog(t, "sglang_ascend_nerdctl_healthy.txt"), enginelog.DialectSGLang, proto.ErrorEngineHealthTimeout},
		"the starved start, which did not say": {fieldLog(t, "sglang_ascend_udocker_tp4_starved.txt"), enginelog.DialectSGLang, proto.ErrorEngineHealthTimeout},
		// A container found running and adopted is not a start: its output
		// holds what it logged while it served.
		"the crashed output of an adopted container": {crashed, enginelog.DialectSGLang, proto.ErrorEngineHealthTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			driver := newLivenessDriver() // no process root, as on nerdctl
			driver.logs = tc.logs
			e, log, spec, id := stalledEngine(t, driver, stallProber{})
			// Long enough for the three looks an abort needs to be seen in.
			spec.LogDialect, spec.StartTimeout = tc.dialect, 8
			if tc.wantCode == proto.ErrorEngineStartAborted {
				spec.StartTimeout = 3600
			}
			fresh := !strings.Contains(name, "adopted")
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if e.probeAndReportStartup(ctx, spec, id, fresh, true) {
				t.Fatal("an engine answering 503 was declared ready")
			}
			failed := log.states(proto.InstFailed)
			if len(failed) != 1 || failed[0].RuntimeFailure == nil || failed[0].RuntimeFailure.Code != tc.wantCode {
				t.Fatalf("reports = %+v", log.snapshot())
			}
			if tc.wantCode != proto.ErrorEngineStartAborted {
				return
			}
			f := failed[0].RuntimeFailure
			if want := []string{"RuntimeError: Unsupport run graph with different stream."}; !slices.Equal(f.Evidence, want) || f.Phase != proto.PhaseHealthCheck {
				t.Fatalf("failure = %+v", f)
			}
			if want := "engine reported a failed process during start and did not exit: RuntimeError: Unsupport run graph with different stream."; failed[0].Message != want {
				t.Fatalf("message = %q", failed[0].Message)
			}
		})
	}
}

// An engine that says it gave up and then exits is reported as the exit it
// was: the first sighting only arms the check.
func TestAnEngineThatGaveUpAndExitsIsReportedAsItsExit(t *testing.T) {
	driver := newLivenessDriver()
	driver.logs = fieldLog(t, "sglang_ascend_nerdctl_schedulers_crashed.txt")
	e, _, spec, id := stalledEngine(t, driver, stallProber{})
	spec.LogDialect = enginelog.DialectSGLang
	var before bool
	if _, _, failed := e.startMakesNoProgress(t.Context(), spec, id, nil, &before); failed || !before {
		t.Fatalf("first sighting: failed=%v armed=%v", failed, before)
	}
	if failure, _, failed := e.startMakesNoProgress(t.Context(), spec, id, nil, &before); !failed || failure.Code != proto.ErrorEngineStartAborted {
		t.Fatalf("second sighting: failed=%v failure=%+v", failed, failure)
	}
	// Output that no longer says so disarms it.
	driver.mu.Lock()
	driver.logs = "restarted\n"
	driver.mu.Unlock()
	if _, _, failed := e.startMakesNoProgress(t.Context(), spec, id, nil, &before); failed || before {
		t.Fatalf("after other output: failed=%v armed=%v", failed, before)
	}
}

// What a start timeout carries for the captured output of deployment 1645's
// third attempt: the three lines the engine printed last (the newest 55
// minutes before its timeout, a worker only then starting to load the
// model), and, where the processes can be read, what they were doing. The
// output is captured; the processes here are the fixture of
// stalledProcFixture, not 1645's own, which were not captured.
func TestTheCapturedStartTimeoutCarriesWhereItStopped(t *testing.T) {
	driver := &stallDriver{livenessDriver: newLivenessDriver(), root: 100}
	driver.logs = fieldLog(t, "vllm_ascend_udocker_tp4_starved.txt")
	prober := stallProber{metrics: idleCards(0, 1, 2, 3)}
	prober.metrics[2].Values["gpuworks_gpu_utilization"] = 80 // no stall: this is the timeout
	e, log, spec, id := stalledEngine(t, driver, prober)
	// Two samples, so the tracer's share of a core is known.
	spec.LogDialect, spec.StartTimeout = enginelog.DialectVLLM, 6
	if e.probeAndReportStartup(t.Context(), spec, id, true, true) {
		t.Fatal("an engine answering 503 was declared ready")
	}
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || failed[0].RuntimeFailure == nil || failed[0].RuntimeFailure.Code != proto.ErrorEngineHealthTimeout {
		t.Fatalf("reports = %+v", log.snapshot())
	}
	f := failed[0].RuntimeFailure
	want := []string{
		"(Worker_TP1 pid=3432365) INFO 11-19 13:22:03 [compilation.py:1091] Using OOT custom backend for compilation.",
		"(Worker_TP0 pid=3425484) INFO 11-19 13:22:03 [model_runner_v1.py:3614] Starting to load model /model...",
		"(Worker_TP0 pid=3425484) INFO 11-19 13:22:33 [modelslim_config.py:584] Applied hf_to_vllm_mapper to quant_description keys",
	}
	if !slices.Equal(f.Evidence, want) || len(f.Causes) != 0 {
		t.Fatalf("evidence = %q\ncauses = %+v", f.Evidence, f.Causes)
	}
	for _, part := range []string{"HTTP 503", "; processes: 2 in tracing stop (VLLM::Worker_TP0, VLLM::Worker_TP1), 3 sleeping; tracer proot-arm64 running at 0% of a core"} {
		if !strings.Contains(f.Detail, part) {
			t.Fatalf("detail %q lacks %q", f.Detail, part)
		}
	}
	if !strings.HasPrefix(failed[0].Message, "health check timed out: ") || strings.Contains(failed[0].Message, "processes:") {
		t.Fatalf("message = %q", failed[0].Message)
	}
}
