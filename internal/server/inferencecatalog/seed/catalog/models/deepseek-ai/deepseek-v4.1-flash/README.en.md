# DeepSeek-V4.1-Flash

Source: [SGLang cookbook](https://docs.sglang.io/cookbook/autoregressive/DeepSeek/DeepSeek-V4_1). Single node: 8 H200 or 4 B200/B300/GB300 GPUs. The mixed FP8/FP4 checkpoint occupies about 510.3 GB on disk; allow additional VRAM, host RAM and download space. Uses the dedicated dev-dsv41 image. Hopper and Blackwell require different kernels.

Upstream marks these recipes verified; the 4× B200 profile was measured on this platform (below), the 8× H200 profile is untested.

AMD, Ascend, Hygon, PPU and Apple are outside this template’s coverage.

## Measured (4× B200, TP4, DSPARK, 2026-10-19)

Vast container node, runveil runtime. Chat, thinking mode (`chat_template_kwargs.thinking=true`) and tool calls passed; single stream ~258 tok/s over 964 output tokens, TTFT 0.49 s through the gateway, DSPARK accept length 2.0–3.7; ~21 minutes from start to ready (JIT kernel compile). No multi-concurrency run yet; 8× H200 not measured.
