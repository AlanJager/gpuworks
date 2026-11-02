# GLM-5.3-Flash

Original FP8 weights (~328.3 GB). The NVIDIA profiles follow the [SGLang recipe](https://docs.sglang.io/cookbook/autoregressive/GLM/GLM-5.3-Flash): H200/H20 use TileLang DSA with a BF16 KV cache, B200/B300 use TRT-LLM DSA with an FP8 KV cache; both accept 4 or 8 GPUs on one node (TP = EP = GPU count). Upstream publishes 8-GPU commands only; the 4-GPU variant follows its GB300 TP4 recipe. 4 MI355X GPUs use the [vLLM ROCm recipe](https://recipes.vllm.ai/zai-org/GLM-5.3-Flash) with the ROCm 10 nightly image.

H20 shares the Hopper profile. With the default parameters, the 96 GB variant needs 8 GPUs; 4 GPUs fail memory admission. The 141 GB variant can be evaluated with 4 or 8 GPUs. Admission uses the capacity reported by the node. This describes the SGLang profile; H20 inference with SGLang has not been validated on hardware.

## vLLM profiles (Hopper × 8, TP8)

vLLM is the only engine on this platform that supports sleep/wake, so Hopper also has two single-node, 8-GPU, TP8 vLLM profiles. Arguments follow the [vLLM recipe](https://recipes.vllm.ai/zai-org/GLM-5.3-Flash) (FlashInfer autotune off, the `glm47` tool and reasoning parsers, `VLLM_ENGINE_READY_TIMEOUT_S=3600`), rewritten for vLLM 0.30.0: the recipe's `--tokenizer-mode glm5_next` does not exist in 0.30.0 (it is not among the allowed values) and is left out, and `--disable-flashinfer-autotune` is spelled `--no-enable-flashinfer-autotune` there. The image is the platform's own `gpuworks-runner:cuda13.0-vllm0.30.0-r2` (vLLM 0.30.0), not the upstream nightly: it registers `Glm5NextForConditionalGeneration` and ran on 8 × H20 141 GB. Both profiles require exactly 8 GPUs; other counts are rejected. The FP8 weights are the same checkpoint as the SGLang profiles above.

| Profile | Cards | Context |
| --- | --- | --- |
| `fp8-vllm-nvidia-hopper-141g-tp8` | H200, H20 141 GB (each card >= 120 GiB) | the model's native 1M; no `--max-model-len` |
| `fp8-vllm-nvidia-hopper-80-96g-tp8` | H100 80 GB, H20 96 GB | capped at 256K (`--max-model-len 262144`) |

Memory per card (TP8, `gpu_memory_utilization` at the deployment default 0.9; the 38.96 GiB of weights per card is measured, the other columns are derived from card capacity at 0.9 and are not measured):

| Card | Capacity | 0.9 budget | Weights | Budget minus weights (activations, CUDA graphs, KV) | Platform admission requirement |
| --- | --- | --- | --- | --- | --- |
| H200 / H20 141 GB | ~140 GiB | ~126 GiB | 38.96 GiB | ~87 GiB | ~43 GiB |
| H20 96 GB | ~95.6 GiB | ~86 GiB | 38.96 GiB | ~47 GiB | ~43 GiB |
| H100 80 GB | ~79.6 GiB | ~71.6 GiB | 38.96 GiB | ~33 GiB | ~43 GiB |

The KV cache is small: in the config only 11 of the 45 layers (`full_attn_layers`) keep a per-token KV cache; the other 34 are linear attention with a length-independent state. On the 80 GB profile the constraint is therefore the activation and CUDA-graph headroom, not KV; the 256K cap leaves room for that rather than reflecting a KV shortfall. H100 80 GB has the least headroom; if start-up runs out of memory, lower `gpu_memory_utilization` first.

Upstream options that are not enabled: MTP speculative decoding (`--speculative-config`), FP8 KV (`--kv-cache-dtype fp8`) and `VLLM_USE_RUST_FRONTEND` (whether the Rust frontend serves the sleep development endpoints has not been checked). Add them through the deployment form's extra arguments if needed.

## Measured (vLLM, 8× H20 141 GB, TP8, 2026-10-31)

Docker runtime, `gpuworks-runner:cuda13.0-vllm0.30.0-r2`, model on NFS (~830 MB/s), sleep mode on. Chat output was correct. Weights loaded in 501 s; cold start to ready took ~17 minutes, including the TileLang kernel compile on first start. Sleep/wake (L1, engine-reported): sleep 1.9-2.1 s, wake 2.9-3.8 s. L2 wake reloads the weights and takes over 10 minutes, so it is not practical. The H200 and H100 / H20 96 GB profiles were only checked offline (catalog and admission); they have not been run on hardware, and the headroom figures above are derived.

The Ascend profile follows the vLLM-Ascend v0.30.0rc2 A3 recipe: one 16-die
910C node, TP16/EP16, W8A8 weights, INT8 KV cache and 1M context. It pins the
complete ModelScope snapshot. MTP and the KDA decode graph still depend on
unmerged upstream fixes, so the primary profile disables speculative decoding
and graph compilation and runs eagerly.

On 2026-10-23 this profile started with the image's native behavior on 16× 910C
with native Docker. All 16 ranks initialized, and platform health, `/v1/models`
(with 1M context) and a real chat request passed. Docker does not inject the
runveil/PRoot compatibility script. This does not establish long-context,
concurrency, multimodal or performance acceptance.

The runveil/PRoot path has not passed. Compatibility fixes can advance rank and
group initialization, but the tested environment still hits CANN Fill/Transpose
tiling registration failures or PRoot thread starvation, so it is not considered
real-node usable.

## Measured (4× B200, TP4/EP4, 2026-10-20)

Vast container node, runveil runtime, `gpuworks-runner:cuda13.0-sglang0.5.21-r2`, EAGLE MTP 5/1/6. Chat, thinking, tool calls and image input passed. Each request: ~1.5k input tokens, exactly 512 output tokens, measured against the engine on the node after warm-up:

| Concurrency | Total output | Per-stream decode | Mean TTFT | Mean TPOT |
| --- | --- | --- | --- | --- |
| 1 | 241 tok/s | 279 tok/s | 0.29 s | 3.6 ms |
| 4 | 652 tok/s | 193 tok/s | 0.44 s | 5.2 ms |
| 16 | 1418 tok/s | 97 tok/s | 0.35 s | 10.3 ms |

Startup to ready took ~13 minutes under runveil; the first batches of a new shape still compile kernels (the first two concurrency-16 rounds saw 5–10 s TTFT). 4× H200 has not been measured yet.

Existing EXL3 TensorFold profiles remain under `mia-ailab/glm-5.3-flash`. Ascend coverage is limited to the 910C Docker case above; runveil has not passed. Hygon, PPU and Apple are not covered.

## Ascend 910C tuned recipes (Graph / DFlash)

Single-node text inference with pinned official vLLM Ascend, W8A8 weights, INT8 KV cache and decode graphs. Two choices are available:

- `w8a8-vllm-910c-dflash5-16npu`: high-performance TP2/DP8 with five-token Modal DFlash. Installs a checked source overlay and downloads a pinned draft model.
- `w8a8-vllm-910c-stock-graph-16npu` and `...-8npu`: stock Graph without custom patches or a draft model; TP2/DP8 and TP4/DP2 respectively.

Each logical device needs at least 64 GiB. Eight dual-die physical cards usually expose 16 logical NPUs. Four 64 GiB devices cannot hold the approximately 309 GiB W8A8 weights. The 8-device profile does not imply capacity for 32 independent long histories.

All profiles use a 139264-token context limit, 2048-token prefill chunks, a 512-token long-prefill threshold, and four sequences per DP rank. History reuse requires the same prefix to return to the same DP rank and depends on observed checkpoint alignment and retention. A nominal 96K warm history does not guarantee exactly 32K uncached tokens.

### High-performance DFlash measurements

Measured on the pinned new image on 2026-10-25: 16 logical devices, TP2/DP8, 32 independent sessions, 131072 input tokens, a 98304-token warm history per session, mixed Chinese/English/code, temperature 0.6. One synchronized wave, no retries, 4096 actual output tokens per request. `X-data-parallel-rank` pins each history and measurement to the same DP rank.

| Decode TPS, min / median / max | Requests reaching 40 TPS | Median TTFT | Output / wave wall time | Observed cached / uncached input per request |
| --- | --- | --- | --- | --- |
| **37.91 / 41.93 / 48.63** | 25 / 32 | 30.59 s | **945.80 tokens/s** | 95744 / 35328 tokens |

The recipe uses `enable_fused_mc2=1`, without MegaMoE or an injected CANN precompilation step. Decode TPS uses actual token increments after the first token bundle. Wave throughput includes prefill and waiting, excluding history warmup. All DP ranks replayed graphs; 32/32 requests and cache gates passed. Cold, partial-hit and full-hit known-answer probes passed, as did 32 concurrent warmups and 64 multi-position answers. Exact input token hashes match the older high-performance mixed-workload test.

This single wave does not establish 32 requests each sustaining 40 TPS: seven requests were below that threshold. The new image has not completed sustained-load qualification; directional answer checks do not establish full business accuracy. Stock Graph below uses 1024 output tokens, so the tables cannot directly establish a speedup ratio.

### Stock Graph measurements

Measured on 2026-10-25: 131072 input tokens, an independent 98304-token warm history per request, mixed Chinese/English/code, one synchronized wave without retries, 1024 actual output tokens per request. `X-data-parallel-rank` pins each history and subsequent request to the same DP rank.

| Logical devices / concurrency | Per-request decode TPS, min / median / max | Median TTFT | Output / wave wall time | Observed cached tokens per request |
| --- | --- | --- | --- | --- |
| 16 / 32 | 25.35 / 25.73 / 26.03 | 29.00 s | 475.91 tokens/s | 97152 |
| 8 / 4 | 31.10 / 31.19 / 31.29 | 12.47 s | 90.43 tokens/s | 97920 |

Decode TPS excludes the first token bundle. Wave throughput includes prefill and waiting but excludes history warmup. Actual uncached input was 33920 and 33152 tokens respectively. All DP ranks replayed graphs; cold, partial-hit and full-hit known-answer checks passed. Multi-position retrieval passed 64/64 and 8/8 answers respectively. These are single-wave results, not sustained-load qualification or evidence of 32 requests each sustaining 40 TPS.

### Image and runtime

The manifest pins the ARM64 official `nightly-releases-v0.30.0-a3` image built on 2026-10-24 with CANN 9.2.0. Host drivers and firmware must match it. All profiles use the V1 runner and `FULL_DECODE_ONLY`, without `--enforce-eager`. Stock Graph explicitly disables DSA context parallel: enabling it failed cold and warm 128K known-answer probes in the tested TP2/DP8 configuration. Disabling it passed the same-input control.

Stock capture sizes are TP-aligned and fit the per-rank scheduler limit. DFlash captures `[6,12,24]`, including five draft tokens per request. The DFlash overlay adapts auxiliary hidden states, draft weight alignment and causality, mixed KV views, DSA indexing and prefix checkpoint retention. Its 15 modules preserve upstream convolution padding fixes. Startup checks all input and payload hashes before changing container files; an incompatible image fails closed. Reviewable patches and rebuild instructions are in `build/inference-templates/glm53-910c/README.md`. Do not upgrade by changing only the image tag.

The official guide uses a different TP16/DP1 topology and describes MTP. Our high-performance profile uses Modal DFlash; the stock profiles use neither.

- [Official model guide](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/GLM5.3-Flash.html)
- [Official compatibility matrix](https://docs.vllm.ai/projects/ascend/en/latest/community/versioning_policy.html)
- [Upstream graph-padding fix](https://github.com/vllm-project/vllm-ascend/pull/18878)

The callable engine passes Ascend-specific INT8 KV options to vLLM. Profiles expose `/health`, streaming OpenAI chat and `/metrics`. vLLM answers under the deployment's served name (`${SERVED_MODEL_NAME}`).

### Historical DFlash results

On the older image, 32 concurrent code-review requests with 131072 input and 4096 output tokens achieved **39.65 / 41.81 / 48.07** min/median/max decode TPS. The final prefix-retention configuration on mixed Chinese/English/code achieved **35.96 / 41.50 / 50.90 TPS**, with 21/32 requests reaching 40 TPS. Different workloads must be reported separately. Continuous closed-loop mixed traffic experienced prefill interference; single-wave decode rates do not establish sustained service performance.
