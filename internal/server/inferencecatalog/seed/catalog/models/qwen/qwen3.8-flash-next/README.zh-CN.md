# Qwen3.8-Flash-Next

[官方 SGLang 配方](https://docs.sglang.io/cookbook/autoregressive/Qwen/Qwen3.8-Flash-Next)。原始 FP8 权重约 185.5 GB；单节点 H200 × 4 或 MI350X/MI355X × 8。使用配方专用镜像。NVIDIA 启用原生 MTP，AMD 使用 AITER 配方。

现有 Apple GGUF 和 NVIDIA Strata 模板保持独立；NVIDIA H200 × 4 配方已在本平台实测（见下），AMD 配方尚未实测。昇腾官方 W8A8 配方只支持 A3，尚未完成权重 revision 核对，不能使用本 FP8 模板代替。海光和 PPU 未核实可执行配方。

## 实测（H200 × 4，TP4，2026-10-19）

Vast 容器节点、runveil 运行时。对话、思考、工具调用通过；单路 1500 token 输出约 272 token/s，TTFT 0.71 s（经网关），NEXTN 接受长度 1.7–3.95；启动到就绪约 9 分钟。未做多并发压测。
