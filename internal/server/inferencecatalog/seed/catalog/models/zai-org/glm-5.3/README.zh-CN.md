# GLM-5.3

[官方 vLLM 配方](https://recipes.vllm.ai/zai-org/GLM-5.3)。默认仓库是 FP8（约 755.6 GB），不是 GLM-5.2 的 BF16。单节点 TP8；NVIDIA 限 H200/H20-3e（141 GB）/B200/B300，96 GB 的 H20 放不下 FP8 权重，AMD 限 MI300X/MI325X/MI350X/MI355X。已配置推理与工具解析器、原生 MTP；上下文上限分别为 256K / 512K，仍受实际 KV 空间限制。

新增模板通过离线检查不代表真机验收。昇腾配方需要额外 overlay，本模板尚未覆盖；海光、PPU、Apple 也未声明支持。
