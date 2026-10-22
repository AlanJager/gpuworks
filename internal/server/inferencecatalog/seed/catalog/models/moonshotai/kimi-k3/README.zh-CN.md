# Kimi-K3

新增 AMD 配方来自 [vLLM 官方文档](https://recipes.vllm.ai/moonshotai/Kimi-K3)：单节点 MI355X × 8，原始 MXFP4 权重约 1.56 TB，使用 ROCm 10 nightly 镜像。预留权重加载所需的主机内存与磁盘空间；模板把上下文限制在 128K，不默认开启外置 draft 或主机 KV offload。

此 AMD 配方尚未完成本平台真机验收。已有 NVIDIA 配方保持原样，不能据此声称每种 NVIDIA 卡都已验证。昇腾官方方案涉及专用镜像、W4A8 权重和多节点通信；海光、PPU、Apple 未核实满足本平台模板契约的配方。
