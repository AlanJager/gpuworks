# GLM-5.3-Flash

Original FP8 weights (~328.3 GB). The NVIDIA profiles follow the [SGLang recipe](https://docs.sglang.io/cookbook/autoregressive/GLM/GLM-5.3-Flash): H200 uses TileLang DSA with a BF16 KV cache, B200/B300 use TRT-LLM DSA with an FP8 KV cache; both accept 4 or 8 GPUs on one node (TP = EP = GPU count). Upstream publishes 8-GPU commands only; the 4-GPU variant follows its GB300 TP4 recipe. 4 MI355X GPUs use the [vLLM ROCm recipe](https://recipes.vllm.ai/zai-org/GLM-5.3-Flash) with the ROCm 10 nightly image.

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
