package aihelper

import (
	"regexp"
	"strings"
)

// Finding is a known failure a failed instance's logs match. Code is stable:
// the client renders the title and the suggested fix from it in its own
// locale. The LLM never sees a finding as a fact - it gets Hypothesis to
// verify against the evidence (see SeedHypotheses).
type Finding struct {
	Code     string     `json:"code"`
	Evidence []Evidence `json:"evidence"`
}

// Evidence is one line that matched a signature. Index is the line's position
// in the slice Match was given, so a caller can point back into its own view.
type Evidence struct {
	Index int    `json:"index"`
	Line  string `json:"line"`
}

// maxEvidence bounds the lines kept per finding: the ones nearest the failure,
// which is where the cause usually prints.
const maxEvidence = 3

type signature struct {
	code string
	// match fires on any line it matches unless exclude also matches it.
	match   *regexp.Regexp
	exclude *regexp.Regexp
	// hypothesis is what the LLM is asked to verify, in English: the model
	// answers in the user's language whatever the prompt's.
	hypothesis string
}

func re(pattern string) *regexp.Regexp { return regexp.MustCompile(`(?i)` + pattern) }

// signatures is the known-failure table. Order is the order findings are
// reported in; a line may match several (an OOM during NCCL setup is both).
var signatures = []signature{
	{
		code:       "kv_cache_insufficient",
		match:      re(`larger than the maximum number of tokens that can be stored in KV cache|KV cache is needed, which is larger than the available KV cache memory|No available memory for the cache blocks|Insufficient memory for KV cache|increase --mem-fraction-static|not enough memory.*kv cache`),
		hypothesis: "The KV cache does not fit what is left after the weights: max_model_len (or the requested sequence length) needs more KV cache than the GPU memory budget leaves. Check the weight size, max_model_len and gpu_memory_utilization; fixes are a smaller --max-model-len, a higher memory utilization, or a larger tensor-parallel size.",
	},
	{
		code:       "cuda_oom",
		match:      re(`CUDA out of memory|OutOfMemoryError|CUDA error: out of memory|NPU out of memory|out of memory.*(GPU|device)`),
		hypothesis: "The process ran out of accelerator memory. Check whether the weights alone fit one GPU's share (weight size / tensor-parallel size), whether another process holds memory on the same GPU, and whether max_model_len or gpu_memory_utilization leave too little headroom.",
	},
	{
		code:       "shm_insufficient",
		match:      re(`failed to extend /dev/shm|creating shared memory segment|/dev/shm has only|No space left on device.*shm|shm_open|unable to (allocate|open) shared memory|Bus error|shared memory.*(insufficient|too small|exhausted)`),
		hypothesis: "Shared memory (/dev/shm) is too small for the engine's inter-process or NCCL traffic. Check the container's shm size; fixes are a larger --shm-size / ipc=host, or disabling NCCL shared-memory transport.",
	},
	{
		code:       "nccl_failure",
		match:      re(`NCCL error|ncclInternalError|ncclSystemError|ncclUnhandledCudaError|ncclRemoteError|ncclInvalidUsage|Watchdog caught collective operation timeout|ProcessGroupNCCL.*(timeout|timed out|error)|NCCL WARN|HCCL function error|hccl api failed|Communication_Error`),
		hypothesis: "Collective communication (NCCL, or HCCL on Ascend) between accelerators or nodes failed. Check the topology and P2P support, the network interface it picked (NCCL_SOCKET_IFNAME / HCCL_SOCKET_IFNAME, IB/RoCE devices), firewalls between nodes, and whether every rank started. A shared-memory error underneath points at /dev/shm instead.",
	},
	{
		code:       "disk_full",
		match:      re(`No space left on device|disk quota exceeded`),
		exclude:    re(`shm`),
		hypothesis: "A disk filled up (model cache, image layers or container writable layer). Check free space on the node's model storage and container runtime directories.",
	},
	{
		code:       "port_in_use",
		match:      re(`address already in use|EADDRINUSE|port is already allocated`),
		hypothesis: "The serving port is already taken by another process or container on the node. Check what listens on the instance's port.",
	},
	{
		code:       "image_pull_denied",
		match:      re(`pull access denied|unauthorized: authentication required|manifest unknown|repository does not exist|failed to resolve reference|denied: requested access|ErrImagePull`),
		hypothesis: "The container image could not be pulled: wrong name/tag, a private registry without credentials, or an unreachable registry. Check the image reference and registry credentials on the node.",
	},
	{
		code:       "dtype_unsupported",
		match:      re(`Bfloat16 is only supported on GPUs with compute capability|bfloat16 is not supported|does not support (bf16|bfloat16|fp8)|fp8 .*not supported`),
		hypothesis: "The GPU does not support the requested dtype (for example bfloat16 below compute capability 8.0). Fix by serving with --dtype float16 or using a supported GPU.",
	},
	{
		code:       "gpu_arch_unsupported",
		match:      re(`no kernel image is available for execution on the device|is not compatible with the current PyTorch installation|sm_[0-9]+ is not compatible|requires compute capability|unsupported (gpu|compute capability)`),
		hypothesis: "The image's kernels were not built for this GPU architecture. Check the GPU's compute capability against the image/engine build; fixes are an image built for that architecture or a different GPU.",
	},
	{
		code:       "trust_remote_code_required",
		match:      re(`trust_remote_code|requires you to execute the configuration file in that repo`),
		hypothesis: "The model ships custom code and the engine refused to load it without --trust-remote-code. Check whether the model's repository is trusted before enabling it.",
	},
	{
		code:       "model_arch_unsupported",
		match:      re(`Model architectures? \[.*\] (are|is) not supported|are not supported for now|Unsupported architecture|Unknown model architecture|does not recognize this architecture`),
		hypothesis: "The engine version does not support this model architecture. Check the architecture in config.json against the engine version; fixes are a newer engine image or another engine.",
	},
	{
		code:       "model_files_missing",
		match:      re(`does not appear to have a file named|Can't load (the )?(config|tokenizer)|No such file or directory: .*(config\.json|tokenizer|safetensors|\.bin)|Incorrect path_or_model_id|Invalid repository ID or local directory specified|Could not detect config format|is not a local folder and is not a valid model identifier|Unrecognized model in`),
		hypothesis: "The model directory the engine was pointed at is missing files (config.json, tokenizer or weights) or the path is wrong. Check the model path in the launch command and whether the download completed.",
	},
	{
		code:       "hf_download_failed",
		match:      re(`Repository Not Found|GatedRepoError|gated repo|401 Client Error|403 Client Error|LocalEntryNotFoundError|couldn't connect to 'https://huggingface\.co'|huggingface\.co.*(timed out|Max retries|Connection)`),
		hypothesis: "Downloading from the model hub failed: a wrong repository id, a gated model without an accepted license/token, or no network route to the hub. Check the repository id, token and the node's network/proxy.",
	},
}

// SignatureCodes lists every code Match can report, in table order.
func SignatureCodes() []string {
	out := make([]string, len(signatures))
	for i, s := range signatures {
		out[i] = s.code
	}
	return out
}

// Match runs the known-failure table over lines (oldest first). Each code is
// reported once, with the matching lines nearest the end.
func Match(lines []string) []Finding {
	var out []Finding
	for _, s := range signatures {
		var ev []Evidence
		for i, line := range lines {
			if !s.match.MatchString(line) || (s.exclude != nil && s.exclude.MatchString(line)) {
				continue
			}
			ev = append(ev, Evidence{Index: i, Line: line})
		}
		if len(ev) == 0 {
			continue
		}
		if len(ev) > maxEvidence {
			ev = ev[len(ev)-maxEvidence:]
		}
		out = append(out, Finding{Code: s.code, Evidence: ev})
	}
	return out
}

// SeedHypotheses renders findings for the LLM as hypotheses to verify, never
// as conclusions: a rule that misfires must not become the answer.
func SeedHypotheses(findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Rule-based hypotheses (unverified - confirm or reject each against the evidence and say which):\n")
	for _, f := range findings {
		for _, s := range signatures {
			if s.code == f.Code {
				b.WriteString("- [" + f.Code + "] " + s.hypothesis + "\n")
				for _, e := range f.Evidence {
					b.WriteString("    evidence: " + clip(e.Line, maxLineChars) + "\n")
				}
			}
		}
	}
	return b.String()
}

// maxLineChars bounds one log line handed to the model; a minified JSON blob
// or a progress bar redrawn on one line is otherwise unbounded.
const maxLineChars = 400

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
