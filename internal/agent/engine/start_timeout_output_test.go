package engine

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// The output of GWKS-354: an SGLang engine that never became healthy printed
// optional-import warnings and then one Gloo line over and over. No line of
// it names an error.
var hungStartOutput = "No module named 'cutlass'\n" +
	"Loading with HF_TOKEN=SECRET-hf from the launch script\n" +
	strings.Repeat("[Gloo] Rank 0 is connected to 0 peer ranks. Expected number of connected peer ranks is : 0\n", 12)

var hungStartEvidence = []string{
	"No module named 'cutlass'",
	"Loading with HF_TOKEN=<redacted> from the launch script",
	"[Gloo] Rank 0 is connected to 0 peer ranks. Expected number of connected peer ranks is : 0",
}

// A leader whose health check times out while its container keeps running
// carries what the container printed last as evidence, the repeated line
// once. The message stays the health check's: a hang's last line is not its
// reason.
func TestHealthTimeoutCarriesTheLastOutputAsEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	driver := newLivenessDriver()
	driver.logs = hungStartOutput
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("hung-start", serverPort(t, server))
	spec.StartTimeout = 1
	name := containerName(spec.UUID)
	if _, err := driver.Run(t.Context(), runtime.WorkloadSpec{Name: name}); err != nil {
		t.Fatal(err)
	}
	if e.probeAndReport(t.Context(), spec, name) {
		t.Fatal("an engine answering 503 was declared ready")
	}
	failed := log.states(proto.InstFailed)
	if len(failed) != 1 || failed[0].RuntimeFailure == nil {
		t.Fatalf("reports = %+v", log.snapshot())
	}
	f := failed[0].RuntimeFailure
	if f.Code != proto.ErrorEngineHealthTimeout || len(f.Causes) != 0 {
		t.Fatalf("failure = %+v", f)
	}
	if !slices.Equal(f.Evidence, hungStartEvidence) {
		t.Fatalf("evidence = %q, want %q", f.Evidence, hungStartEvidence)
	}
	message := failed[0].Message
	if !strings.HasPrefix(message, "health check timed out: ") || !strings.Contains(message, "HTTP 503") || strings.Contains(message, "Gloo") || strings.Contains(message, "SECRET") {
		t.Fatalf("message = %q", message)
	}
}

// A headless worker's readiness timeout carries the same evidence, and its
// message is unchanged.
func TestWorkerReadinessTimeoutCarriesTheLastOutputAsEvidence(t *testing.T) {
	driver := &loggingHeadlessDriver{headlessTestDriver: &headlessTestDriver{status: runtime.Status{Exists: true, Running: true}}, logs: hungStartOutput}
	var statuses []proto.InstanceStatus
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir())
	e.headlessProbeInterval, e.headlessStableChecks = time.Millisecond, 1<<30
	e.probeAndReport(t.Context(), proto.InstanceDesiredState{UUID: "worker-hung", Headless: true, NodeRank: 1, StartTimeout: 1}, "worker")
	st := failedReport(t, statuses)
	if f := st.RuntimeFailure; f.Code != proto.ErrorWorkerReadinessTimeout || !slices.Equal(f.Evidence, hungStartEvidence) {
		t.Fatalf("failure = %+v", f)
	}
	if st.Message != "headless worker readiness timed out" {
		t.Fatalf("message = %q", st.Message)
	}
}

// What a still-running container's output adds to a failure. Only a startup
// that timed out without a known failure or an error line gets its last
// lines; an error line and a known failure are reported exactly as they are
// for a container that is merely unhealthy, and output that cannot be read
// or is empty adds nothing.
func TestOutputOfAContainerThatIsStillRunning(t *testing.T) {
	const withError = "INFO loading\nRuntimeError: NPU graph capture failed\nINFO still waiting\nINFO still waiting\n"
	const known = "INFO loading\nNCCL WARN socket timeout\nINFO still waiting\n"
	for name, tc := range map[string]struct {
		logs         string
		logsErr      error
		timedOut     bool
		wantEvidence []string
		wantCauses   []string
	}{
		"timed out on ordinary lines":         {logs: hungStartOutput, timedOut: true, wantEvidence: hungStartEvidence},
		"unhealthy on ordinary lines":         {logs: hungStartOutput},
		"timed out with an error line":        {logs: withError, timedOut: true, wantEvidence: []string{"RuntimeError: NPU graph capture failed"}},
		"unhealthy with an error line":        {logs: withError, wantEvidence: []string{"RuntimeError: NPU graph capture failed"}},
		"timed out with a known failure":      {logs: known, timedOut: true, wantCauses: []string{enginelog.CodeNCCLFailure}},
		"unhealthy with a known failure":      {logs: known, wantCauses: []string{enginelog.CodeNCCLFailure}},
		"timed out, output unreadable":        {logsErr: errors.New("configured logging driver does not support reading"), timedOut: true},
		"timed out, no output":                {timedOut: true},
		"timed out, only blank lines":         {logs: "\n \r\n\n", timedOut: true},
		"timed out, fewer lines than the cap": {logs: "INFO waiting\nINFO waiting\n", timedOut: true, wantEvidence: []string{"INFO waiting"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newHermeticEngine(&loggingHeadlessDriver{headlessTestDriver: &headlessTestDriver{}, logs: tc.logs, logsErr: tc.logsErr}, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir())
			failure := proto.RuntimeFailure{Code: proto.ErrorEngineHealthTimeout, Phase: proto.PhaseHealthCheck}
			e.attachOutput(t.Context(), "c", 0, &failure, tc.timedOut)
			if !slices.Equal(failure.Evidence, tc.wantEvidence) {
				t.Fatalf("evidence = %q, want %q", failure.Evidence, tc.wantEvidence)
			}
			var causes []string
			for _, c := range failure.Causes {
				causes = append(causes, c.Code)
			}
			if !slices.Equal(causes, tc.wantCauses) {
				t.Fatalf("causes = %q, want %q", causes, tc.wantCauses)
			}
			for _, line := range failure.Evidence {
				if strings.Contains(line, "SECRET") || len([]rune(line)) > proto.MaxFailureEvidenceChars+1 {
					t.Fatalf("evidence line %q", line)
				}
			}
			if len(failure.Evidence) > proto.MaxFailureEvidence {
				t.Fatalf("%d evidence lines", len(failure.Evidence))
			}
		})
	}
}
