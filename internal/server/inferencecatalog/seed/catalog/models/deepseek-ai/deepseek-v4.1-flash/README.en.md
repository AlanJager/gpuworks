# DeepSeek-V4.1-Flash

Sources: the [SGLang cookbook](https://docs.sglang.io/cookbook/autoregressive/DeepSeek/DeepSeek-V4_1)
and the [vLLM-Ascend DeepSeek-V4.1-Flash guide](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/DeepSeek-V4.1-Flash.html).
The NVIDIA profiles use 8 H200 or 4 B200/B300/GB300 GPUs. The mixed FP8/FP4 checkpoint
occupies about 510.3 GB on disk; allow additional VRAM, host RAM and download space.
They require the dedicated dev-dsv41 image. Hopper and Blackwell use different kernels.

The Ascend A3 profile uses `Eco-Tech/DeepSeek-V4.1-Flash-w8a8` (about 797.6 GB) on one
64 GB × 16-logical-device server with DP4 × TP4. Engram tables stay in host memory and
the default context is 150K. Because the checkpoint nearly fills the node's HBM, the
profile allows up to 36,000 seconds for engine readiness.

Upstream marks the NVIDIA recipes verified. This platform measured the 4× B200 profile
(below); the 8× H200 and Ascend A3 profiles still need real deployment and chat runs.

AMD, Hygon, PPU and Apple are outside this template's coverage.

## Measured (4× B200, TP4, DSPARK, 2026-10-19)

Vast container node, runveil runtime. Chat, thinking mode (`chat_template_kwargs.thinking=true`) and tool calls passed; single stream ~258 tok/s over 964 output tokens, TTFT 0.49 s through the gateway, DSPARK accept length 2.0–3.7; ~21 minutes from start to ready (JIT kernel compile). No multi-concurrency run yet; 8× H200 not measured.
