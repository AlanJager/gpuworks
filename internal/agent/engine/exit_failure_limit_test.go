package engine

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// The output failed dGPU instances left: three whose memory limit was too
// small for the engine to start (on Ascend the interposer's refusal above the
// allocator error the engine raised for it), and the Ascend one
// (limitAscendLog) whose die had run out of streams.
const (
	limitVLLMLog   = "INFO 11-19 17:46:02 [api_server.py:1] vLLM API server\nValueError: Free memory on device cuda:0 (0.75/1.0 GiB) on startup is less than desired GPU memory utilization (0.9, 0.9 GiB). Decrease GPU memory utilization or reduce GPU memory used by other processes.\n"
	limitPPULog    = "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 20.00 MiB. GPU 0 has a total capacity of 1024.00 MiB of which 0 bytes is free. Including non-PyTorch memory, this process has 1024.00 MiB memory in use.\n"
	limitAscendLog = "[2026-11-19 11:38:39.750] [ERROR] [eNPU] [vCANN_RT] [179:281453904523520:device.c:59] Call rtSetDevice fails, ret:507033.\n" +
		"(EngineCore pid=179) [PID: 179] 2026-11-19-11:38:38.325.811 Resource_Error(EE1023): Alloc Stream resource failed. Reason: Too many streams are created.\n" +
		"(EngineCore pid=179) RuntimeError: ExchangeDevice:../torch_npu/csrc/core/npu/sys_ctrl/npu_sys_ctrl.cpp:285 NPU function error: c10_npu::SetDevice(device), error code is 507033\n"
	limitMallocLog = "[2026-11-19 13:43:04.868] [ERROR] [eNPU] [vCANN_RT] [179:281470230524160:memory_ledger.c:352] Out of memory! Request:312475648 B, accounted:8466202624 B, quota:8589934592 B.\n" +
		"[2026-11-19 13:43:04.868] [ERROR] [eNPU] [vCANN_RT] [179:281470230524160:memory.c:50] Memory reservation rejected for requested:312475648.\n" +
		"(EngineCore pid=179) RuntimeError: malloc:../torch_npu/csrc/core/npu/NPUCachingAllocator.cpp:1335 NPU function error: params.err, error code is 300000\n"
)

func exitedInstanceReport(t *testing.T, logs string, vramLimitBytes int64) proto.InstanceStatus {
	t.Helper()
	driver := &loggingHeadlessDriver{headlessTestDriver: &headlessTestDriver{status: runtime.Status{Exists: true, ExitCode: 1}}, logs: logs}
	var statuses []proto.InstanceStatus
	e := newHermeticEngine(driver, func(st proto.InstanceStatus) { statuses = append(statuses, st) }, slog.Default(), t.TempDir())
	e.headlessProbeInterval = time.Millisecond
	e.probeAndReport(t.Context(), proto.InstanceDesiredState{UUID: "limited", Headless: true, NodeRank: 1, StartTimeout: 1, VRAMLimitBytes: vramLimitBytes}, "worker")
	return failedReport(t, statuses)
}

// A KV cache that does not fit a limited instance (GWKS-423: 1 GiB limit on a
// 910B4, 0.09 GiB left for a KV cache of 0.44 GiB) names the limit first and
// keeps the KV cache cause; the message carries the engine's own line, whose
// figures are the only ones known.
func TestAKVCacheShortageUnderADGPULimitNamesTheLimitAndKeepsTheEnginesLine(t *testing.T) {
	const line = "ValueError: To serve at least one request with the models's max seq len (40960), (0.44 GiB KV cache is needed, which is larger than the available KV cache memory (0.09 GiB). Based on the available memory, the estimated maximum model length is 8368."
	st := exitedInstanceReport(t, "INFO 11-23 11:20:01 [api_server.py:1] vLLM API server\n"+line+"\n", 1<<30)
	f := st.RuntimeFailure
	if f.Code != proto.ErrorContainerExited || len(f.Causes) != 2 || f.Causes[0].Code != enginelog.CodeDGPUMemoryLimit || f.Causes[1].Code != enginelog.CodeKVCacheInsufficient {
		t.Fatalf("failure = %+v", f)
	}
	if c := f.Causes[0]; c.LimitBytes != 1<<30 || c.NeededBytes != 0 || c.Device != nil {
		t.Fatalf("limit cause = %+v", c)
	}
	if want := "headless worker exited (code 1): dGPU memory limit reached: the engine could not get the memory it needs within its 1.0 GiB limit (" + line + ")"; st.Message != want {
		t.Fatalf("message = %q, want %q", st.Message, want)
	}
	// Without a limit the same exit is the KV cache alone.
	if f := exitedInstanceReport(t, line+"\n", 0).RuntimeFailure; len(f.Causes) != 1 || f.Causes[0].Code != enginelog.CodeKVCacheInsufficient {
		t.Fatalf("unlimited failure = %+v", f)
	}
}

// A start that failed for memory on an instance with a dGPU memory limit is
// reported as the limit: the cause the assistant's findings are read from,
// its numbers where the engine printed them, and a message that says so.
func TestStartFailureUnderADGPULimitIsReportedAsTheLimit(t *testing.T) {
	const gib = int64(1) << 30
	device := func(i int) *int { return &i }
	cases := map[string]struct {
		logs        string
		limit       int64
		wantDevice  *int
		wantNeeded  int64
		wantMessage string
	}{
		"NVIDIA vLLM startup check": {
			logs: limitVLLMLog, limit: gib, wantDevice: device(0), wantNeeded: 1234803097,
			wantMessage: "headless worker exited (code 1): dGPU memory limit reached on GPU 0: the engine needed 1.1 GiB, its limit is 1.0 GiB",
		},
		"PPU PyTorch allocator": {
			logs: limitPPULog, limit: gib, wantDevice: device(0), wantNeeded: gib + 20<<20,
			wantMessage: "headless worker exited (code 1): dGPU memory limit reached on GPU 0: the engine needed 1.02 GiB, its limit is 1.00 GiB",
		},
		"Ascend interposer refusal": {
			logs: limitMallocLog, limit: 8 * gib, wantNeeded: 8466202624 + 312475648,
			wantMessage: "headless worker exited (code 1): dGPU memory limit reached: the engine needed 8.2 GiB, its limit is 8.0 GiB",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st := exitedInstanceReport(t, tc.logs, tc.limit)
			f := st.RuntimeFailure
			if f.Code != proto.ErrorContainerExited || len(f.Causes) != 1 {
				t.Fatalf("reason = %+v", f)
			}
			c := f.Causes[0]
			if c.Code != enginelog.CodeDGPUMemoryLimit || c.LimitBytes != tc.limit || c.NeededBytes != tc.wantNeeded || len(c.Evidence) != 1 || !strings.Contains(tc.logs, c.Evidence[0]+"\n") {
				t.Fatalf("cause = %+v", c)
			}
			if (c.Device == nil) != (tc.wantDevice == nil) || (c.Device != nil && *c.Device != *tc.wantDevice) {
				t.Fatalf("device = %v, want %v", c.Device, tc.wantDevice)
			}
			if st.Message != tc.wantMessage {
				t.Fatalf("message = %q, want %q", st.Message, tc.wantMessage)
			}
			// The engine's own error stays the failure's evidence.
			lines := strings.Split(strings.TrimSpace(tc.logs), "\n")
			if len(f.Evidence) != 1 || f.Evidence[0] != lines[len(lines)-1] {
				t.Fatalf("evidence = %q", f.Evidence)
			}
		})
	}
}

// The same output from an instance on a whole GPU is not a quota problem:
// a card PyTorch ran out of is out of GPU memory, and free memory below the
// utilization is reported by the engine's own error, unclassified.
func TestTheSameStartFailureOnAWholeGPUIsNotTheLimit(t *testing.T) {
	cases := map[string]struct {
		logs      string
		wantCodes []string
		wantTail  string
	}{
		"NVIDIA vLLM startup check": {limitVLLMLog, nil, "is less than desired GPU memory utilization (0.9, 0.9 GiB). Decrease GPU memory utilization or reduce GPU memory used by other processes."},
		"PPU PyTorch allocator":     {limitPPULog, []string{enginelog.CodeCUDAOOM}, "this process has 1024.00 MiB memory in use."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			st := exitedInstanceReport(t, tc.logs, 0)
			var codes []string
			for _, c := range st.RuntimeFailure.Causes {
				codes = append(codes, c.Code)
				if c.Device != nil || c.LimitBytes != 0 || c.NeededBytes != 0 {
					t.Fatalf("cause carries limit numbers: %+v", c)
				}
			}
			if !slices.Equal(codes, tc.wantCodes) {
				t.Fatalf("causes = %v, want %v", codes, tc.wantCodes)
			}
			const prefix = "headless worker exited (code 1): "
			if len(st.Message) < len(prefix)+len(tc.wantTail) || st.Message[:len(prefix)] != prefix || st.Message[len(st.Message)-len(tc.wantTail):] != tc.wantTail {
				t.Fatalf("message = %q", st.Message)
			}
		})
	}
}

// An Ascend device that would not set up is not the limit, with one or
// without: the die had run out of streams, which no code names, so the
// engine's own error is the reason and nothing is classified.
func TestAscendStreamExhaustionIsNotReportedAsTheLimit(t *testing.T) {
	for _, limit := range []int64{0, 1 << 30} {
		st := exitedInstanceReport(t, limitAscendLog, limit)
		if len(st.RuntimeFailure.Causes) != 0 {
			t.Fatalf("limit %d: causes = %+v, want none", limit, st.RuntimeFailure.Causes)
		}
		if want := "headless worker exited (code 1): (EngineCore pid=179) RuntimeError: ExchangeDevice:../torch_npu/csrc/core/npu/sys_ctrl/npu_sys_ctrl.cpp:285 NPU function error: c10_npu::SetDevice(device), error code is 507033"; st.Message != want {
			t.Fatalf("limit %d: message = %q", limit, st.Message)
		}
	}
}

// A limited instance that timed out while still running, or stopped while
// serving, is read against its limit as well.
func TestLimitIsReadOutsideAStartupExitToo(t *testing.T) {
	const gib = int64(1) << 30
	e := newHermeticEngine(&loggingHeadlessDriver{headlessTestDriver: &headlessTestDriver{}, logs: limitPPULog}, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir())
	timeout := proto.RuntimeFailure{Code: proto.ErrorWorkerReadinessTimeout, Phase: proto.PhaseHealthCheck}
	e.attachOutput(t.Context(), "c", gib, &timeout, true)
	if len(timeout.Causes) != 1 || timeout.Causes[0].Code != enginelog.CodeDGPUMemoryLimit {
		t.Fatalf("running container: %+v", timeout.Causes)
	}
	serving, msg := e.exitedFailure(t.Context(), "c", runtime.Status{Exists: true, ExitCode: 1}, gib, proto.PhaseServing, "oom", "container exited while running (code 1)")
	if len(serving.Causes) != 1 || serving.Causes[0].Code != enginelog.CodeDGPUMemoryLimit || msg != "container exited while running (code 1): dGPU memory limit reached on GPU 0: the engine needed 1.02 GiB, its limit is 1.00 GiB" {
		t.Fatalf("serving exit: %+v, message %q", serving.Causes, msg)
	}
}
