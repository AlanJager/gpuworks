# GLM-5.3-Flash · Ascend 910C

Single-node text inference with pinned official vLLM Ascend, W8A8 weights, INT8 KV cache and decode graphs. Two choices are available:

- `w8a8-vllm-910c-dflash5-16npu`: high-performance TP2/DP8 with five-token Modal DFlash. Installs a checked source overlay and downloads a pinned draft model.
- `w8a8-vllm-910c-stock-graph-16npu` and `...-8npu`: stock Graph without custom patches or a draft model; TP2/DP8 and TP4/DP2 respectively.

Each logical device needs at least 64 GiB. Eight dual-die physical cards usually expose 16 logical NPUs. Four 64 GiB devices cannot hold the approximately 309 GiB W8A8 weights. The 8-device profile does not imply capacity for 32 independent long histories.

All profiles use a 139264-token context limit, 2048-token prefill chunks, a 512-token long-prefill threshold, and four sequences per DP rank. History reuse requires the same prefix to return to the same DP rank and depends on observed checkpoint alignment and retention. A nominal 96K warm history does not guarantee exactly 32K uncached tokens.

## High-performance DFlash measurements

Measured on the pinned new image on 2026-10-25: 16 logical devices, TP2/DP8, 32 independent sessions, 131072 input tokens, a 98304-token warm history per session, mixed Chinese/English/code, temperature 0.6. One synchronized wave, no retries, 4096 actual output tokens per request. `X-data-parallel-rank` pins each history and measurement to the same DP rank.

| Decode TPS, min / median / max | Requests reaching 40 TPS | Median TTFT | Output / wave wall time | Observed cached / uncached input per request |
| --- | --- | --- | --- | --- |
| **37.91 / 41.93 / 48.63** | 25 / 32 | 30.59 s | **945.80 tokens/s** | 95744 / 35328 tokens |

The recipe uses `enable_fused_mc2=1`, without MegaMoE or an injected CANN precompilation step. Decode TPS uses actual token increments after the first token bundle. Wave throughput includes prefill and waiting, excluding history warmup. All DP ranks replayed graphs; 32/32 requests and cache gates passed. Cold, partial-hit and full-hit known-answer probes passed, as did 32 concurrent warmups and 64 multi-position answers. Exact input token hashes match the older high-performance mixed-workload test.

This single wave does not establish 32 requests each sustaining 40 TPS: seven requests were below that threshold. The new image has not completed sustained-load qualification; directional answer checks do not establish full business accuracy. Stock Graph below uses 1024 output tokens, so the tables cannot directly establish a speedup ratio.

## Stock Graph measurements

Measured on 2026-10-25: 131072 input tokens, an independent 98304-token warm history per request, mixed Chinese/English/code, one synchronized wave without retries, 1024 actual output tokens per request. `X-data-parallel-rank` pins each history and subsequent request to the same DP rank.

| Logical devices / concurrency | Per-request decode TPS, min / median / max | Median TTFT | Output / wave wall time | Observed cached tokens per request |
| --- | --- | --- | --- | --- |
| 16 / 32 | 25.35 / 25.73 / 26.03 | 29.00 s | 475.91 tokens/s | 97152 |
| 8 / 4 | 31.10 / 31.19 / 31.29 | 12.47 s | 90.43 tokens/s | 97920 |

Decode TPS excludes the first token bundle. Wave throughput includes prefill and waiting but excludes history warmup. Actual uncached input was 33920 and 33152 tokens respectively. All DP ranks replayed graphs; cold, partial-hit and full-hit known-answer checks passed. Multi-position retrieval passed 64/64 and 8/8 answers respectively. These are single-wave results, not sustained-load qualification or evidence of 32 requests each sustaining 40 TPS.

## Image and runtime

The manifest pins the ARM64 official `nightly-releases-v0.30.0-a3` image built on 2026-10-24 with CANN 9.2.0. Host drivers and firmware must match it. All profiles use the V1 runner and `FULL_DECODE_ONLY`, without `--enforce-eager`. Stock Graph explicitly disables DSA context parallel: enabling it failed cold and warm 128K known-answer probes in the tested TP2/DP8 configuration. Disabling it passed the same-input control.

Stock capture sizes are TP-aligned and fit the per-rank scheduler limit. DFlash captures `[6,12,24]`, including five draft tokens per request. The DFlash overlay adapts auxiliary hidden states, draft weight alignment and causality, mixed KV views, DSA indexing and prefix checkpoint retention. Its 15 modules preserve upstream convolution padding fixes. Startup checks all input and payload hashes before changing container files; an incompatible image fails closed. Reviewable patches and rebuild instructions are in `build/inference-templates/glm53-910c/README.md`. Do not upgrade by changing only the image tag.

The official guide uses a different TP16/DP1 topology and describes MTP. Our high-performance profile uses Modal DFlash; the stock profiles use neither.

- [Official model guide](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/GLM5.3-Flash.html)
- [Official compatibility matrix](https://docs.vllm.ai/projects/ascend/en/latest/community/versioning_policy.html)
- [Upstream graph-padding fix](https://github.com/vllm-project/vllm-ascend/pull/18878)

The callable engine passes Ascend-specific INT8 KV options to vLLM. Profiles expose `/health`, streaming OpenAI chat and `/metrics`. Served names are `glm53-flash-910c-stock-graph` and `glm53-flash-910c-dflash`.

## Historical DFlash results

On the older image, 32 concurrent code-review requests with 131072 input and 4096 output tokens achieved **39.65 / 41.81 / 48.07** min/median/max decode TPS. The final prefix-retention configuration on mixed Chinese/English/code achieved **35.96 / 41.50 / 50.90 TPS**, with 21/32 requests reaching 40 TPS. Different workloads must be reported separately. Continuous closed-loop mixed traffic experienced prefill interference; single-wave decode rates do not establish sustained service performance.
