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
