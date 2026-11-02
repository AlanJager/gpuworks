# Qwen3.8-Flash-Next

[Official SGLang recipe](https://docs.sglang.io/cookbook/autoregressive/Qwen/Qwen3.8-Flash-Next). Native FP8 weights (~185.5 GB), single-node 4 H200/H20 or 8 MI350X/MI355X GPUs. Uses the dedicated recipe images. NVIDIA enables native MTP; AMD uses AITER.

Existing Apple GGUF and NVIDIA Strata profiles remain separate. The NVIDIA 4× H200 profile was measured on this platform (below); the AMD profile is untested. The Ascend profile uses vLLM-Ascend's experimental W8A8-MTP recipe: immutable revision `c76aa96a…`, eight 64 GiB A3 logical devices, TP8. The NVIDIA/AMD FP8 recipe is not a substitute. Hygon/PPU recipes remain unconfirmed.

The 96 GB and 141 GB H20 variants share the 4-GPU profile; admission uses the capacity reported by the node. This describes the SGLang profile; H20 inference with SGLang has not been validated on hardware.

## vLLM profile (Hopper × 8, TP8)

vLLM is the only engine on this platform that supports sleep/wake, so H200, H20 (96/141 GB) and H100 80 GB also have one single-node, 8-GPU, TP8 vLLM profile, `bf16-vllm-nvidia-hopper-tp8`; other GPU counts are rejected. Arguments follow the [vLLM recipe](https://recipes.vllm.ai/Qwen/Qwen3.8-Flash-Next) (`--max-num-seqs 256`, the `qwen3_coder` tool parser, the `qwen3` reasoning parser, `VLLM_ENGINE_READY_TIMEOUT_S=1800`). The image is the platform's own `gpuworks-runner:cuda13.0-vllm0.30.0-r2` (vLLM 0.30.0), not the upstream nightly: it registers `Qwen4ExpForConditionalGeneration`, and the recipe states that stable vLLM >= 0.30.0 is supported.

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

Docker runtime, `gpuworks-runner:cuda13.0-vllm0.30.0-r2`, BF16 weights on NFS (~830 MB/s), sleep mode on, `--disable-custom-all-reduce`. Chat output was correct. With the weights in the page cache, start to ready took ~3.5 minutes; a cold read from NFS takes longer. Sleep/wake (L1, engine-reported): sleep 1.7-1.9 s, wake 2.9-3.7 s. L2 wake reloads the weights and took 489 s, so it is not practical. H100 80 GB, H20 96 GB and H200 were only checked offline (catalog and admission); they have not been run on hardware, and the headroom figures above are derived.

## Measured (4× H200, TP4, 2026-10-19)

Vast container node, runveil runtime. Chat, thinking and tool calls passed; single stream ~272 tok/s over 1,500 output tokens, TTFT 0.71 s through the gateway, NEXTN accept length 1.7–3.95; ~9 minutes from start to ready. No multi-concurrency run yet.

## Measured (8× 910C, TP8, 2026-10-22)

Self-hosted 910C node, Docker runtime, vLLM-Ascend's `qwen3.8-next-a3` image,
pinned here by digest. Automatic placement left out the die whose ACL context
check fails and ran on the other eight. The deployment reached running;
`/health`, `/v1/models` and an OpenAI-compatible chat request all returned
HTTP 200 with a correct answer. No throughput or multi-concurrency run yet.
