package aihelper

import (
	"strings"
	"testing"
)

// Every signature fires on a real-format line and stays quiet on a near miss
// a healthy engine prints.
func TestMatchRecognisesEachSignature(t *testing.T) {
	cases := []struct {
		code string
		hit  string
		miss string
	}{
		{"kv_cache_insufficient", "ValueError: The model's max seq len (32768) is larger than the maximum number of tokens that can be stored in KV cache (12000).", "INFO: # GPU blocks: 4096, maximum number of tokens that can be stored in KV cache: 65536"},
		{"cuda_oom", "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB.", "INFO: Memory profiling takes 3.2 seconds"},
		{"shm_insufficient", "RuntimeError: unable to open shared memory object </torch_1_2> in read-write mode", "INFO: shm broadcast enabled"},
		{"nccl_failure", "torch.distributed.DistBackendError: NCCL error in: ProcessGroupNCCL.cpp:1970, unhandled system error, ncclSystemError", "INFO: NCCL version 2.21.5+cuda12.4"},
		{"disk_full", "OSError: [Errno 28] No space left on device: '/root/.cache/huggingface'", "INFO: Disk usage 40%"},
		{"port_in_use", "ERROR: [Errno 98] error while attempting to bind on address ('0.0.0.0', 8000): address already in use", "INFO: Uvicorn running on http://0.0.0.0:8000"},
		{"image_pull_denied", "Error response from daemon: pull access denied for registry.local/vllm, repository does not exist or may require 'docker login'", "Status: Downloaded newer image for vllm/vllm-openai:latest"},
		{"dtype_unsupported", "ValueError: Bfloat16 is only supported on GPUs with compute capability of at least 8.0. Your Tesla T4 GPU has compute capability 7.5.", "INFO: dtype=torch.bfloat16"},
		{"gpu_arch_unsupported", "RuntimeError: CUDA error: no kernel image is available for execution on the device", "INFO: Using FlashAttention backend"},
		{"trust_remote_code_required", "ValueError: Loading /models/x requires you to execute the configuration file in that repo on your local machine.", "INFO: Loading model weights"},
		{"model_arch_unsupported", "ValueError: Model architectures ['FooForCausalLM'] are not supported for now.", "INFO: Resolved architecture: Qwen3ForCausalLM"},
		{"model_files_missing", "OSError: /models/qwen does not appear to have a file named config.json.", "INFO: Loading config.json"},
		{"hf_download_failed", "huggingface_hub.errors.RepositoryNotFoundError: 401 Client Error. Repository Not Found for url: https://huggingface.co/api/models/x", "INFO: Downloading from https://huggingface.co"},
	}
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.code] = true
		got := Match([]string{c.hit})
		if len(got) == 0 || got[0].Code != c.code {
			t.Errorf("%s: Match(hit) = %+v", c.code, got)
		}
		for _, f := range Match([]string{c.miss}) {
			t.Errorf("%s: near miss %q matched %s", c.code, c.miss, f.Code)
		}
	}
	for _, code := range SignatureCodes() {
		if !covered[code] {
			t.Errorf("signature %s has no test case", code)
		}
	}
}

// An engine that merely uses /dev/shm (a KV offload file, Ray's object store
// notice) is not a shared-memory failure.
func TestMatchIgnoresInformationalShmMentions(t *testing.T) {
	for _, line := range []string{
		"(EngineCore) INFO [shared_offload_region.py:71] Created mmap file /dev/shm/vllm_offload_1.mmap (4.29 GB)",
		"INFO: shm broadcast enabled",
	} {
		if got := Match([]string{line}); len(got) != 0 {
			t.Errorf("Match(%q) = %+v", line, got)
		}
	}
	got := Match([]string{"HCCL function error: hcclCommInitRootInfoConfig(numRanks, &rootInfo, rank, config, &(comm->hcclComm_)), error code is 1"})
	if len(got) != 1 || got[0].Code != "nccl_failure" {
		t.Fatalf("HCCL failure = %+v, want nccl_failure", got)
	}
}

// "No space left on device" on /dev/shm is a shared-memory problem, not a
// full disk.
func TestMatchSeparatesShmFromDiskFull(t *testing.T) {
	got := Match([]string{"OSError: [Errno 28] No space left on device: '/dev/shm/nccl-abc'"})
	if len(got) != 1 || got[0].Code != "shm_insufficient" {
		t.Fatalf("Match = %+v, want shm_insufficient only", got)
	}
}

func TestMatchKeepsNewestEvidenceAndIndexes(t *testing.T) {
	lines := []string{"start", "CUDA out of memory #1", "x", "CUDA out of memory #2", "CUDA out of memory #3", "CUDA out of memory #4"}
	got := Match(lines)
	if len(got) != 1 || len(got[0].Evidence) != maxEvidence {
		t.Fatalf("Match = %+v", got)
	}
	if ev := got[0].Evidence; ev[0].Index != 3 || ev[2].Index != 5 || ev[2].Line != "CUDA out of memory #4" {
		t.Fatalf("evidence = %+v, want the newest three with their indexes", ev)
	}
	if Match(nil) != nil {
		t.Fatal("Match(nil) should report nothing")
	}
}

func TestSeedHypothesesAreMarkedUnverified(t *testing.T) {
	if SeedHypotheses(nil) != "" {
		t.Fatal("no findings should render nothing")
	}
	long := "CUDA out of memory " + strings.Repeat("y", 1000)
	h := SeedHypotheses(Match([]string{long}))
	if !strings.Contains(h, "unverified") || !strings.Contains(h, "[cuda_oom]") || !strings.Contains(h, "evidence: CUDA out of memory") {
		t.Fatalf("hypotheses = %q", h)
	}
	if strings.Contains(h, strings.Repeat("y", maxLineChars)) {
		t.Fatal("evidence line was not clipped")
	}
}

func TestClipKeepsUTF8Boundaries(t *testing.T) {
	got := clip("显存不足显存不足", 7)
	if got != "显存…" {
		t.Fatalf("clip = %q", got)
	}
}
