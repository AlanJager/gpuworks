# Qwen3.8-Flash-Next

[Official SGLang recipe](https://docs.sglang.io/cookbook/autoregressive/Qwen/Qwen3.8-Flash-Next). Native FP8 weights (~185.5 GB), single-node 4 H200 or 8 MI350X/MI355X GPUs. Uses the dedicated recipe images. NVIDIA enables native MTP; AMD uses AITER.

Existing Apple GGUF and NVIDIA Strata profiles remain separate. The NVIDIA 4× H200 profile was measured on this platform (below); the AMD profile is untested. The Ascend W8A8 recipe is A3-only and its immutable weight revision is not yet checked; this FP8 profile is not a substitute. Hygon/PPU recipes remain unconfirmed.

## Measured (4× H200, TP4, 2026-10-19)

Vast container node, runveil runtime. Chat, thinking and tool calls passed; single stream ~272 tok/s over 1,500 output tokens, TTFT 0.71 s through the gateway, NEXTN accept length 1.7–3.95; ~9 minutes from start to ready. No multi-concurrency run yet.
