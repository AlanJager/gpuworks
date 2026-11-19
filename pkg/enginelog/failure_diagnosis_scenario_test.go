package enginelog_test

import (
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// The generic wrapper Ascend and PPU engine builds end a failed start with,
// in the spelling and prefix the engine prints (the same line as the fixture
// in internal/agent/app/background_test.go).
const genericWrapper = "(APIServer pid=83923) [ERROR] 2026-09-18-20:39:28 (PID:83923) ERR99999 UNKNOWN applicaiton exception" //nolint:misspell // External engine output.

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// GWKS-353: a start that ran out of KV cache memory was diagnosed as
// TRUST_REMOTE_CODE_REQUIRED, with the engine's echo of its own arguments as
// the evidence, and its error was the generic wrapper. The output here is the
// captured SGLang argument echo (it names trust_remote_code), the captured
// vLLM KV cache failure, and the wrapper line the accelerator builds add.
func TestKVCacheExhaustionIsNotDiagnosedFromTheArgumentEcho(t *testing.T) {
	echo := readFixture(t, "sglang-nextn-tp4.txt")
	if !strings.Contains(echo, "trust_remote_code") {
		t.Fatal("the captured argument echo no longer names trust_remote_code; the control is void")
	}
	log := echo + "\n" + readFixture(t, "vllm-kv-cache-failure.txt") + "\n" + genericWrapper + "\n"

	// The classification below is what mainline already did for this output
	// (the argument echo stopped matching before this case was written); it
	// is here to keep the scenario whole, not as a test of RootCause.
	causes := enginelog.Classify(log)
	if len(causes) != 1 || causes[0].Code != enginelog.CodeKVCacheInsufficient {
		t.Fatalf("causes = %+v, want only %s", causes, enginelog.CodeKVCacheInsufficient)
	}
	if len(causes[0].Evidence) == 0 || len(causes[0].Evidence) > enginelog.MaxEvidence {
		t.Fatalf("evidence = %d lines", len(causes[0].Evidence))
	}
	for _, line := range causes[0].Lines() {
		if !strings.Contains(line, "KV cache is needed") || strings.Contains(line, "server_args") {
			t.Fatalf("evidence is not the failure's own line: %q", line)
		}
	}
	root := enginelog.RootCause(log)
	if !strings.HasPrefix(root, "ValueError: To serve at least one request") || !strings.Contains(root, "16.92 GiB KV cache is needed") {
		t.Fatalf("root cause = %q, want the KV cache ValueError", root)
	}
}

// A specific known failure outranks the generic wrapper as the reported
// error even when the engine printed it as a plain log line, not as the last
// line of a traceback. With nothing known above it, the wrapper is what
// there is.
func TestKnownFailureOutranksTheGenericWrapper(t *testing.T) {
	for _, tc := range []struct {
		name, log, want string
	}{
		{
			name: "plain KV cache line",
			log:  "(EngineCore pid=77) [ERROR] 2026-11-16-10:02:11 (PID:77, Device:0, RankID:0) No available memory for the cache blocks. Try increasing `gpu_memory_utilization` when initializing the engine.\n" + genericWrapper,
			want: "No available memory for the cache blocks. Try increasing `gpu_memory_utilization` when initializing the engine.",
		},
		{
			name: "KV cache line logged as a warning",
			log:  "(EngineCore pid=77) [WARNING] 2026-11-16-10:02:11 (PID:77) No available memory for the cache blocks. Try increasing `gpu_memory_utilization` when initializing the engine.\n" + genericWrapper,
			want: genericWrapper,
		},
		{
			// A wrapper that says where its cause is keeps standing when
			// only a warning the engine got past is above it.
			name: "benign warning above a vLLM wrapper",
			log:  "(Worker pid=5) NCCL WARN MPI not found, falling back\nINFO loading\n(APIServer pid=1) RuntimeError: Engine core initialization failed. See root cause above. Failed core proc(s): {}",
			want: "(APIServer pid=1) RuntimeError: Engine core initialization failed. See root cause above. Failed core proc(s): {}",
		},
		{
			name: "argument echo above a vLLM wrapper",
			log:  "INFO args: {'note': 'address already in use is retried'}\nERROR 11-05 13:39:46 [core.py:1183] EngineCore failed to start.",
			want: "ERROR 11-05 13:39:46 [core.py:1183] EngineCore failed to start.",
		},
		{
			name: "argument echo naming a known failure above the generic wrapper",
			log:  "INFO args: {'note': 'address already in use is retried'}\n" + genericWrapper,
			want: genericWrapper,
		},
		{
			name: "argument echo then wrapper",
			log:  "INFO non-default args: {'trust_remote_code': True}\n" + genericWrapper,
			want: genericWrapper,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := enginelog.RootCause(tc.log); got != tc.want {
				t.Fatalf("RootCause = %q, want %q", got, tc.want)
			}
		})
	}
}

// A real refusal is still diagnosed when the engine also echoed the option,
// and its evidence is the refusal, not the echo.
func TestRemoteCodeRefusalIsDiagnosedBesideTheArgumentEcho(t *testing.T) {
	const refusal = "ValueError: Loading /model requires you to execute the configuration file in that repo on your local machine. Make sure you have read the code there to avoid malicious use, then set the option `trust_remote_code=True` to remove this error."
	log := readFixture(t, "sglang-nextn-tp4.txt") + "\n" + refusal + "\n" + genericWrapper + "\n"
	causes := enginelog.Classify(log)
	if len(causes) != 1 || causes[0].Code != enginelog.CodeTrustRemoteCodeRequired || len(causes[0].Evidence) != 1 || causes[0].Evidence[0].Line != refusal {
		t.Fatalf("causes = %+v", causes)
	}
	if root := enginelog.RootCause(log); !strings.HasPrefix(root, "ValueError: Loading /model requires you to execute") {
		t.Fatalf("root cause = %q", root)
	}
}
