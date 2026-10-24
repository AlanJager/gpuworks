# GLM-5.3

[Official vLLM recipe](https://recipes.vllm.ai/zai-org/GLM-5.3). The default checkpoint is FP8 (~755.6 GB), not GLM-5.2 BF16. Single-node TP8 on H200, H20-3e (141 GB), B200 or B300, or AMD Instinct MI300X/MI325X/MI350X/MI355X; the 96 GB H20 cannot hold the FP8 weights. Reasoning, tool parsing and native MTP are configured; context limits are 256K / 512K, subject to KV capacity.

Offline checks do not constitute live hardware acceptance. GLM-5.3 non-Flash has no Ascend hardware result on which to publish a profile, so this template has no Ascend spec; Hygon, PPU and Apple are not covered either.
