package enginelog

import (
	"reflect"
	"testing"
)

// The lines failed instances showed when their dGPU memory limit was too
// small for the engine to start.
const (
	vllmFreeMemoryLine  = "ValueError: Free memory on device cuda:0 (0.75/1.0 GiB) on startup is less than desired GPU memory utilization (0.9, 0.9 GiB). Decrease GPU memory utilization or reduce GPU memory used by other processes."
	vllmKVCacheLine     = "ValueError: To serve at least one request with the models's max seq len (40960), (0.44 GiB KV cache is needed, which is larger than the available KV cache memory (0.09 GiB). Based on the available memory, the estimated maximum model length is 8368."
	torchOOMElidedLine  = "torch.OutOfMemoryError: CUDA out of memory ... GPU 0 has a total capacity of 1024.00 MiB ..."
	torchOOMLine        = "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 20.00 MiB. GPU 0 has a total capacity of 1024.00 MiB of which 0 bytes is free. Including non-PyTorch memory, this process has 1024.00 MiB memory in use."
	ascendSetDeviceLine = "(EngineCore pid=179) RuntimeError: ExchangeDevice:../torch_npu/csrc/core/npu/sys_ctrl/npu_sys_ctrl.cpp:285 NPU function error: c10_npu::SetDevice(device), error code is 507033"
	ascendMallocLine    = "RuntimeError: malloc:../torch_npu/csrc/core/npu/NPUCachingAllocator.cpp:1335 NPU function error: params.err, error code is 300000"
	flashinferLine      = "Exception: flashinfer_python is installed with version 0.6.8.post1+v0.1.0.ppu2.1.0, which is less than the minimum required version 0.6.12. Please uninstall the old version and reinstall the latest version"
	engineCoreKilled    = "(APIServer pid=1) RuntimeError: Engine core initialization failed. See root cause above. Failed core proc(s): {'EngineCore': -9}"
)

const testGiB = int64(1) << 30

// On an instance with a dGPU memory limit, each way an engine reports
// running out of memory inside it is the limit, with the numbers the line
// gives, and is the only cause reported for it.
func TestMatchWithLimitReadsTheEnginesOwnReport(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		limit    int64
		device   int
		needed   int64
		fallback string
	}{
		// 0.25 GiB already held of the 1.0 GiB the engine sees, 0.9 GiB asked for.
		{"vLLM startup check", vllmFreeMemoryLine, testGiB, 0, 1234803097, "dGPU memory limit reached on GPU 0: the engine needed 1.1 GiB, its limit is 1.0 GiB"},
		{"PyTorch allocator", torchOOMLine, testGiB, 0, testGiB + 20<<20, "dGPU memory limit reached on GPU 0: the engine needed 1.02 GiB, its limit is 1.00 GiB"},
		{"PyTorch allocator, as the ticket quotes it", torchOOMElidedLine, testGiB, 0, 0, "dGPU memory limit reached on GPU 0: the engine could not get the memory it needs within its 1.0 GiB limit"},
		// The reopened ticket's start: a 1 GiB limit, 0.09 GiB left for a KV cache of 0.44 GiB.
		{"vLLM KV cache check", vllmKVCacheLine, testGiB, DeviceUnknown, testGiB - gibBytes("0.09") + gibBytes("0.44"), "dGPU memory limit reached: the engine needed 1.3 GiB, its limit is 1.0 GiB"},
		{"PyTorch allocator, older wording", "RuntimeError: CUDA out of memory. Tried to allocate 2.00 GiB (GPU 1; 8.00 GiB total capacity; 7.10 GiB already allocated; 512.00 MiB free; 7.20 GiB reserved in total by PyTorch)", 8 * testGiB, 1, 8*testGiB - 512<<20 + 2*testGiB, "dGPU memory limit reached on GPU 1: the engine needed 9.5 GiB, its limit is 8.0 GiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchWithLimit([]string{"INFO start", tc.line}, tc.limit)
			if len(got) != 1 || got[0].Code != CodeDGPUMemoryLimit {
				t.Fatalf("causes = %+v, want %s alone", got, CodeDGPUMemoryLimit)
			}
			c := got[0]
			if c.Device != tc.device || c.NeededBytes != tc.needed || c.LimitBytes != tc.limit {
				t.Fatalf("device %d needed %d limit %d, want %d %d %d", c.Device, c.NeededBytes, c.LimitBytes, tc.device, tc.needed, tc.limit)
			}
			if !reflect.DeepEqual(c.Evidence, []Evidence{{Index: 1, Line: tc.line}}) {
				t.Fatalf("evidence = %+v", c.Evidence)
			}
			if c.Fallback() != tc.fallback {
				t.Fatalf("fallback = %q, want %q", c.Fallback(), tc.fallback)
			}
		})
	}
}

// Without a dGPU memory limit the same lines are not a quota problem: a
// whole card PyTorch ran out of is out of GPU memory, and the rest are what
// they were before, unclassified (free memory below the utilization is
// another process holding the card; an Ascend error code is a device's).
func TestTheSameLinesWithoutALimitAreNotTheLimit(t *testing.T) {
	for line, want := range map[string]string{
		vllmFreeMemoryLine:  "",
		vllmKVCacheLine:     CodeKVCacheInsufficient,
		torchOOMLine:        CodeCUDAOOM,
		torchOOMElidedLine:  CodeCUDAOOM,
		ascendMallocLine:    "",
		ascendSetDeviceLine: "",
	} {
		for _, got := range [][]Cause{Match([]string{line}), MatchWithLimit([]string{line}, 0), Classify(line + "\n")} {
			switch {
			case want == "" && len(got) != 0:
				t.Errorf("%q: causes = %+v, want none", line, got)
			case want != "" && (len(got) != 1 || got[0].Code != want):
				t.Errorf("%q: causes = %+v, want %s alone", line, got, want)
			}
		}
	}
}

// A limited instance whose engine saw a device larger than its limit did not
// run out inside the limit: the card's own exhaustion stays what it is.
func TestALimitIsNotBlamedForADeviceItDoesNotSize(t *testing.T) {
	for line, want := range map[string]string{
		"ValueError: Free memory on device cuda:0 (0.75/23.69 GiB) on startup is less than desired GPU memory utilization (0.9, 21.32 GiB). Decrease GPU memory utilization or reduce GPU memory used by other processes.": "",
		"torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 20.00 MiB. GPU 0 has a total capacity of 23.69 GiB of which 3.25 MiB is free.":                                                                      CodeCUDAOOM,
		"RuntimeError: CUDA error: out of memory": CodeCUDAOOM,
		// 20 GiB left for the KV cache is not a device of 1 GiB.
		"ValueError: To serve at least one request with the models's max seq len (1048576), (36.50 GiB KV cache is needed, which is larger than the available KV cache memory (20.00 GiB). Based on the available memory, the estimated maximum model length is 574000.": CodeKVCacheInsufficient,
		// A KV cache shortage that states no amounts cannot be held against the limit.
		"ValueError: The model's max seq len (32768) is larger than the maximum number of tokens that can be stored in KV cache (12000).": CodeKVCacheInsufficient,
	} {
		got := MatchWithLimit([]string{line}, testGiB)
		switch {
		case want == "" && len(got) != 0:
			t.Errorf("%q under a 1 GiB limit: causes = %+v, want none", line, got)
		case want != "" && (len(got) != 1 || got[0].Code != want):
			t.Errorf("%q under a 1 GiB limit: causes = %+v, want %s alone", line, got, want)
		}
	}
}

// The interposer's own refusal is the limit with or without the engine's
// report beside it, and keeps its numbers; a missing glibc still comes first
// and an unrelated cause is kept.
func TestMatchWithLimitKeepsTheTablesOrderAndTheRefusal(t *testing.T) {
	refusal := "[HAMI-core ERROR (pid:9 thread=1 allocator.c:52)]: Device 1 OOM 9663676416 / 8589934592"
	got := MatchWithLimit([]string{refusal, torchOOMLine}, 8*testGiB)
	if len(got) != 2 || got[0].Code != CodeDGPUMemoryLimit || got[0].Device != 1 || got[0].NeededBytes != 9663676416 || got[1].Code != CodeCUDAOOM {
		t.Fatalf("with a refusal: %+v", got)
	}
	got = MatchWithLimit([]string{
		"libc.so.6: version `GLIBC_2.33' not found (required by /opt/libvruntime.so)",
		"OSError: [Errno 98] address already in use",
		torchOOMLine,
		torchOOMLine,
	}, testGiB)
	var codes []string
	for _, c := range got {
		codes = append(codes, c.Code)
	}
	if !reflect.DeepEqual(codes, []string{CodeGLIBCTooOld, CodeDGPUMemoryLimit, CodePortInUse}) {
		t.Fatalf("codes = %v", codes)
	}
	if !reflect.DeepEqual(got[1].Lines(), []string{torchOOMLine, torchOOMLine}) || got[1].Evidence[1].Index != 3 {
		t.Fatalf("limit evidence = %+v", got[1].Evidence)
	}
}

// The Ascend interposer's refusal is the limit by its own numbers, known
// limit or not, and names no device. The allocator error the engine raises
// for it (error code 300000) is not: without the refusal beside it, nothing
// says the limit refused the allocation.
func TestAscendInterposerRefusalIsTheLimit(t *testing.T) {
	lines := []string{
		"(EngineCore pid=179) INFO 11-19 13:42:59 [worker.py:598] Available KV cache memory: 6.38 GiB",
		"[2026-11-19 13:43:04.868] [ERROR] [eNPU] [vCANN_RT] [179:281470230524160:memory_ledger.c:352] Out of memory! Request:312475648 B, accounted:8466202624 B, quota:8589934592 B.",
		"[2026-11-19 13:43:04.868] [ERROR] [eNPU] [vCANN_RT] [179:281470230524160:memory.c:50] Memory reservation rejected for requested:312475648.",
		"(EngineCore pid=179) " + ascendMallocLine,
	}
	for _, limit := range []int64{0, 8 * testGiB} {
		got := MatchWithLimit(lines, limit)
		if len(got) != 1 || got[0].Code != CodeDGPUMemoryLimit {
			t.Fatalf("limit %d: causes = %+v", limit, got)
		}
		c := got[0]
		if c.Device != DeviceUnknown || c.NeededBytes != 8466202624+312475648 || c.LimitBytes != 8589934592 {
			t.Fatalf("limit %d: cause = %+v", limit, c)
		}
		if !reflect.DeepEqual(c.Evidence, []Evidence{{Index: 1, Line: lines[1]}}) {
			t.Fatalf("limit %d: evidence = %+v", limit, c.Evidence)
		}
		if want := "dGPU memory limit reached: the engine needed 8.2 GiB, its limit is 8.0 GiB"; c.Fallback() != want {
			t.Fatalf("fallback = %q, want %q", c.Fallback(), want)
		}
	}
	for _, limit := range []int64{0, 8 * testGiB} {
		if got := MatchWithLimit([]string{ascendMallocLine}, limit); len(got) != 0 {
			t.Errorf("allocator error alone, limit %d: causes = %+v, want none", limit, got)
		}
	}
}

// An Ascend device that would not set up (507033) is not the limit, even on
// an instance that has one: the output of the 1 GiB instance that showed it
// says the die had run out of streams, and a process without a limit met the
// same error on that die. No code names that, so nothing is reported.
func TestAscendStreamExhaustionUnderALimitIsNotTheLimit(t *testing.T) {
	lines := []string{
		"[2026-11-19 11:38:39.750] [ERROR] [eNPU] [vCANN_RT] [179:281453904523520:device.c:59] Call rtSetDevice fails, ret:507033.",
		"(EngineCore pid=179)         rtSetDevice execution failed, reason=device retain error[FUNC:FuncErrorReason][FILE:error_message_manage.cc][LINE:69]",
		"(EngineCore pid=179) [PID: 179] 2026-11-19-11:38:38.325.811 Resource_Error(EE1023): Alloc Stream resource failed. Reason: Too many streams are created.",
		ascendSetDeviceLine,
		"(EngineCore pid=179) ERROR 11-19 11:38:43 [core.py:1330] [PID: 179] 2026-11-19-11:38:38.325.811 Resource_Error(EE1023): Alloc Stream resource failed. Reason: Too many streams are created.",
		"(EngineCore pid=179) ERROR 11-19 11:38:43 [core.py:1330] RuntimeError: ExchangeDevice:../torch_npu/csrc/core/npu/sys_ctrl/npu_sys_ctrl.cpp:285 NPU function error: c10_npu::SetDevice(device), error code is 507033",
	}
	for _, limit := range []int64{0, testGiB} {
		if got := MatchWithLimit(lines, limit); len(got) != 0 {
			t.Errorf("limit %d: causes = %+v, want none", limit, got)
		}
	}
	for i, line := range lines {
		if got := MatchWithLimit([]string{line}, testGiB); len(got) != 0 {
			t.Errorf("line %d alone: causes = %+v, want none", i, got)
		}
	}
}

// ClassifyWithLimit reads a stopped container's whole output like Classify:
// colour codes do not hide the line and an empty output is its own cause.
func TestClassifyWithLimitReadsAStoppedContainersOutput(t *testing.T) {
	got := ClassifyWithLimit("INFO start\n\x1b[31m"+vllmFreeMemoryLine+"\x1b[0m\n", testGiB)
	if len(got) != 1 || got[0].Code != CodeDGPUMemoryLimit || got[0].Evidence[0].Line != vllmFreeMemoryLine {
		t.Fatalf("causes = %+v", got)
	}
	if got := ClassifyWithLimit("", testGiB); len(got) != 1 || got[0].Code != CodeNoOutput {
		t.Fatalf("empty output = %+v", got)
	}
}

// An image whose dependency is older than the engine needs and an engine
// core killed by a signal are not failures the table names: neither line
// proves a cause it has a code for, with or without a limit.
func TestDependencyMismatchAndKilledCoreStayUnclassified(t *testing.T) {
	for _, line := range []string{flashinferLine, engineCoreKilled} {
		if got := MatchWithLimit([]string{line}, 40*testGiB); len(got) != 0 {
			t.Errorf("%q: causes = %+v, want none", line, got)
		}
	}
	if got := RootCause(flashinferLine + "\n"); got != flashinferLine {
		t.Errorf("root cause = %q, want the dependency error itself", got)
	}
}
