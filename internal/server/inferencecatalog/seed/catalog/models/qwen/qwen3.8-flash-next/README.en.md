# Qwen3.8-Flash-Next

[Official SGLang recipe](https://docs.sglang.io/cookbook/autoregressive/Qwen/Qwen3.8-Flash-Next). Native FP8 weights (~185.5 GB), single-node 4 H200/H20 or 8 MI350X/MI355X GPUs. Uses the dedicated recipe images. NVIDIA enables native MTP; AMD uses AITER.

Existing Apple GGUF and NVIDIA Strata profiles remain separate. The NVIDIA 4× H200 profile was measured on this platform (below); the AMD profile is untested. The Ascend profile uses vLLM-Ascend's experimental W8A8-MTP recipe: immutable revision `c76aa96a…`, eight 64 GiB A3 logical devices, TP8. 910B (A2) has its own single-node 4-card profiles, below. The NVIDIA/AMD FP8 recipe is not a substitute. Hygon/PPU recipes remain unconfirmed.

The 96 GB and 141 GB H20 variants share the 4-GPU profile; admission uses the capacity reported by the node. This describes the SGLang profile; H20 inference with SGLang has not been validated on hardware.

## vLLM profile (Hopper × 8, TP8)

vLLM is the only engine on this platform that supports sleep/wake, so H200, H20 (96/141 GB) and H100 80 GB also have one single-node, 8-GPU, TP8 vLLM profile, `bf16-vllm-nvidia-hopper-tp8`; other GPU counts are rejected. Arguments follow the [vLLM recipe](https://recipes.vllm.ai/Qwen/Qwen3.8-Flash-Next) (`--max-num-seqs 256`, the `qwen3_coder` tool parser, the `qwen3` reasoning parser, `VLLM_ENGINE_READY_TIMEOUT_S=1800`). The image is the platform's own `gpuworks-runner:cuda13.0-vllm0.30.0-r4` (vLLM 0.30.0), not the upstream nightly: it registers `Qwen4ExpForConditionalGeneration`, and the recipe states that stable vLLM >= 0.30.0 is supported.

**The weights are the BF16 `Qwen/Qwen3.8-Flash-Next`, not the FP8 used by the SGLang/AMD/PPU profiles above**: the repository is ~335 GiB (360.0 GB), about 42 GiB per card at TP8 (measured); the 185.5 GB figure above refers to the FP8 repository only. This is the combination that ran on hardware (TP8, no expert parallelism). The upstream vLLM recipe uses FP8 weights and, at TP8, needs `--enable-expert-parallel` (the FP8 128-wide quantization blocks do not split 8 ways); that combination has not been run on this platform, so it is not offered.

`--disable-custom-all-reduce` is required: measured on H20 with sleep mode on, FULL CUDA graph capture fails at `custom_all_reduce.cuh:164 'invalid argument'` without it, and works with it. Sleep off was not tested, so whether the flag is needed without sleep is unknown.

Memory per card (TP8, `gpu_memory_utilization` at the deployment default 0.9; the ~42 GiB of weights per card is measured, the other columns are derived from card capacity at 0.9 and are not measured):

| Card | Capacity | 0.9 budget | Weights | Budget minus weights (activations, CUDA graphs, KV, linear-attention state) | Platform admission requirement |
| --- | --- | --- | --- | --- | --- |
| H200 / H20 141 GB | ~140 GiB | ~126 GiB | ~42 GiB | ~84 GiB | ~47 GiB |
| H20 96 GB | ~95.6 GiB | ~86 GiB | ~42 GiB | ~44 GiB | ~47 GiB |
| H100 80 GB | ~79.6 GiB | ~71.6 GiB | ~42 GiB | ~30 GiB | ~47 GiB |

Context stays at the model's native 256K; `--max-model-len` is not set. The KV cache is small: of 48 layers only 12 use full attention, with just 2 KV heads, and the other 36 are linear attention with a length-independent state, so even 80 GB cards need no shorter context. H100 has the least headroom; if start-up runs out of memory, lower `gpu_memory_utilization` or `--max-num-seqs` first. The upstream recipe also says vLLM offloads the N-gram embedding to host memory by default on H100; this was not verified here, so host-memory use on H100 is unknown, and `--engram-config '{"cpu_offload":true}'` can be added explicitly if needed.

Upstream options that are not enabled: MTP speculative decoding, `--enable-prefix-caching`, FP8 KV and `--attention-config.indexer_kv_dtype fp8` (none verified on 0.30.0). Add them through the deployment form's extra arguments if needed.

## Measured (vLLM, 8× H20 141 GB, TP8, 2026-10-31)

Docker runtime, `gpuworks-runner:cuda13.0-vllm0.30.0-r4`, BF16 weights on NFS (~830 MB/s), sleep mode on, `--disable-custom-all-reduce`. Chat output was correct. With the weights in the page cache, start to ready took ~3.5 minutes; a cold read from NFS takes longer. Sleep/wake (L1, engine-reported): sleep 1.7-1.9 s, wake 2.9-3.7 s. L2 wake reloads the weights and took 489 s, so it is not practical. H100 80 GB, H20 96 GB and H200 were only checked offline (catalog and admission); they have not been run on hardware, and the headroom figures above are derived.

## Measured (4× H200, TP4, 2026-10-19)

Vast container node, runveil runtime. Chat, thinking and tool calls passed; single stream ~272 tok/s over 1,500 output tokens, TTFT 0.71 s through the gateway, NEXTN accept length 1.7–3.95; ~9 minutes from start to ready. No multi-concurrency run yet.

## Measured (8× 910C, TP8, 2026-10-22)

Self-hosted 910C node, Docker runtime, vLLM-Ascend's `qwen3.8-next-a3` image,
pinned here by digest. Automatic placement left out the die whose ACL context
check fails and ran on the other eight. The deployment reached running;
`/health`, `/v1/models` and an OpenAI-compatible chat request all returned
HTTP 200 with a correct answer. No throughput or multi-concurrency run yet.

## Ascend 910B (A2) · 4 cards · TP4

One node with four 910B cards of at least 64 GiB each, TP4 with expert parallelism, on vLLM-Ascend's
official `v0.32.0rc1-openeuler` image (pinned by its multi-arch index digest; the NJU quay mirror and the
public gpuworks copy on Huawei SWR serve the same digest, since quay.io can be very slow from mainland China).
Four profiles:

| Profile | Weights (ModelScope, pinned revision) | Repository size | Speculative decoding |
| --- | --- | --- | --- |
| `int8-vllm-910b-tp4` | `Eco-Tech/Qwen3.8-Flash-Next-w8a8-mtp` `c76aa96a…` | about 245 GB | none |
| `int8-vllm-910b-tp4-mtp` | same | about 245 GB | MTP, 3 draft tokens |
| `w4a8-vllm-910b-tp4` | `Eco-Tech/Qwen3.8-Flash-Next-w4a8` `d75537fd…` | about 194 GB | none |
| `w4a8-vllm-910b-tp4-mtp` | same | about 194 GB | MTP, 3 draft tokens |

Both repositories carry their own BF16 MTP layer, so the MTP profiles download no separate draft. Arguments:
256K context, at most 4 sequences, 8192 tokens per batch, a fixed 20 GiB KV pool per card
(`--kv-cache-memory-bytes`; vLLM then ignores the memory fraction for KV), prefix caching
(`--mamba-cache-mode align`, `--block-size 4096`), up to 32 images per request, a decode-only ACL graph,
async scheduling off, the `qwen3` reasoning parser and the `qwen3_xml` tool parser. The MTP profiles capture
decode graphs at multiples of four (one target plus three draft tokens per sequence) and run the drafter eager,
as vLLM-Ascend's 910C recipe does.

**Host memory**: `--engram-config` keeps the 51B-parameter per-layer embedding table in host memory, one
mapping per rank (`dp_shared_memory=false`), so four ranks need about 96 GiB of free RAM on top of the page
cache for the weight files.

Measured (4× 910B4 64 GiB, ARM64, driver 25.5.0, 2026-11-19): W4A8 without MTP, with these arguments (launched
then from a hand-written command). 18.65 GiB of weights per card, a 20 GiB KV pool (about 1.44M tokens, 5.48
concurrent 256K requests), decode graph capture 83 s, about 9.5 minutes from load to ready; chat requests were
answered, at about 25 tokens/s for one stream. The W8A8 profile's arguments were also run on the same card type
(the user's run record).

MTP measured (910B2 64 GiB x 4, ARM64, `w4a8-vllm-910b-tp4-mtp`, 2026-11-20): 19.88 GiB of
weights per card loaded in 172 s, `torch.compile` 91-106 s. Chat and streaming both answered correctly, at
about 26 tokens/s for one stream (a 300-token answer in 11.4 s). Speculative decoding, from the deltas of
`vllm:spec_decode_num_{draft,accepted}_tokens_total` in `/metrics`: 47%-58% acceptance on ordinary prose
(2.4-2.8 tokens per step), and 88% on highly predictable output such as counting (3.6 per step).
`int8-vllm-910b-tp4-mtp` differs only in weights and quantization and was not measured separately.

**The first request is slow.** Some kernels are compiled just in time for the tensor shapes a request actually
uses, so the first request takes markedly longer than the ones after it; the throughput above is the steady
state once that cache is warm. The cache lives inside the container, so a rebuilt container pays it again.
