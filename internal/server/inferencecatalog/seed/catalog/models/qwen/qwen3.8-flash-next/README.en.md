# Qwen3.8-Flash-Next

[Official SGLang recipe](https://docs.sglang.io/cookbook/autoregressive/Qwen/Qwen3.8-Flash-Next). Native FP8 weights (~185.5 GB), single-node 4 H200/H20 or 8 MI350X/MI355X GPUs. Uses the dedicated recipe images. NVIDIA enables native MTP; AMD uses AITER.

Existing Apple GGUF and NVIDIA Strata profiles remain separate. The NVIDIA 4× H200 profile was measured on this platform (below); the AMD profile is untested. The Ascend profile uses vLLM-Ascend's experimental W8A8-MTP recipe: immutable revision `c76aa96a…`, eight 64 GiB A3 logical devices, TP8. The NVIDIA/AMD FP8 recipe is not a substitute. Hygon/PPU recipes remain unconfirmed.

The 96 GB and 141 GB H20 variants share the 4-GPU profile; admission uses the capacity reported by the node. H20 inference has not been validated on hardware.

## Measured (4× H200, TP4, 2026-10-19)

Vast container node, runveil runtime. Chat, thinking and tool calls passed; single stream ~272 tok/s over 1,500 output tokens, TTFT 0.71 s through the gateway, NEXTN accept length 1.7–3.95; ~9 minutes from start to ready. No multi-concurrency run yet.

## Measured (8× 910C, TP8, 2026-10-22)

Self-hosted 910C node, Docker runtime, vLLM-Ascend's `qwen3.8-next-a3` image,
pinned here by digest. Automatic placement left out the die whose ACL context
check fails and ran on the other eight. The deployment reached running;
`/health`, `/v1/models` and an OpenAI-compatible chat request all returned
HTTP 200 with a correct answer. No throughput or multi-concurrency run yet.
