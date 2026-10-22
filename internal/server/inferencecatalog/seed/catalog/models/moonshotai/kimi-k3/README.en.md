# Kimi-K3

The new AMD profile follows the [official vLLM recipe](https://recipes.vllm.ai/moonshotai/Kimi-K3): one node with 8 MI355X GPUs, ~1.56 TB of original MXFP4 weights, and the ROCm 10 nightly image. Allow sufficient host RAM and disk for loading. Context is capped at 128K; external drafts and host KV offload are not enabled by default.

This AMD profile has no platform hardware acceptance yet. Existing NVIDIA profiles are unchanged and do not imply verification on every NVIDIA card. Ascend requires a dedicated multi-node W4A8 recipe; Hygon, PPU and Apple recipes meeting the platform contract remain unconfirmed.
