# Qwen3.8-Flash-Next

[官方 SGLang 配方](https://docs.sglang.io/cookbook/autoregressive/Qwen/Qwen3.8-Flash-Next)。原始 FP8 权重约 185.5 GB；单节点 H200 × 4 或 MI350X/MI355X × 8。使用配方专用镜像。NVIDIA 启用原生 MTP，AMD 使用 AITER 配方。

现有 Apple GGUF 和 NVIDIA Strata 模板保持独立；NVIDIA H200 × 4 配方已在本平台实测（见下），AMD 配方尚未实测。昇腾档位使用 vLLM-Ascend 官方实验性 W8A8-MTP 配方：固定权重 revision `c76aa96a…`，8 个 64 GiB 的 A3 逻辑设备，TP8。不能使用本页 NVIDIA/AMD 的 FP8 模板代替。海光和 PPU 未核实可执行配方。

## 实测（H200 × 4，TP4，2026-10-19）

Vast 容器节点、runveil 运行时。对话、思考、工具调用通过；单路 1500 token 输出约 272 token/s，TTFT 0.71 s（经网关），NEXTN 接受长度 1.7–3.95；启动到就绪约 9 分钟。未做多并发压测。

## 实测（910C × 8，TP8，2026-10-22）

自有 910C 节点、Docker 运行时，使用 vLLM-Ascend 的 `qwen3.8-next-a3` 镜像（本页按
digest 固定）。自动排布避开了 ACL 上下文检查失败的那张 die，用其余 8 张启动；部署
达到 running，`/health`、`/v1/models` 和 OpenAI 兼容 chat 均返回 200，回答内容正确。
未做吞吐与多并发压测。
