# GLM-5.3-Flash

原始 FP8 权重约 328.3 GB。NVIDIA 配方参照 [SGLang 配方](https://docs.sglang.io/cookbook/autoregressive/GLM/GLM-5.3-Flash)：H200 用 TileLang DSA + BF16 KV，B200/B300 用 TRT-LLM DSA + FP8 KV；两者都接受单节点 4 卡或 8 卡（TP = EP = 卡数）。上游只给出 8 卡命令，4 卡由本平台按 GB300 的 TP4 配方补充。MI355X × 4 使用 [vLLM ROCm 配方](https://recipes.vllm.ai/zai-org/GLM-5.3-Flash)，AMD 使用 ROCm 10 nightly 镜像。

## 实测（B200 × 4，TP4/EP4，2026-10-20）

Vast 容器节点、runveil 运行时、`gpuworks-runner:cuda13.0-sglang0.5.21-r2`、EAGLE MTP 5/1/6。对话、思考、工具调用与图像输入均通过。每个请求约 1.5k 输入 token、固定 512 输出 token，在节点本机直连引擎测得（预热后）：

| 并发 | 总输出吞吐 | 单路解码 | 平均 TTFT | 平均 TPOT |
| --- | --- | --- | --- | --- |
| 1 | 241 token/s | 279 token/s | 0.29 s | 3.6 ms |
| 4 | 652 token/s | 193 token/s | 0.44 s | 5.2 ms |
| 16 | 1418 token/s | 97 token/s | 0.35 s | 10.3 ms |

runveil 下从启动到就绪约 13 分钟；首次遇到新的批形状时还要编译内核，并发 16 的前两轮 TTFT 达 5–10 秒，之后回落。H200 × 4 尚未实测。

已有 GB10 双节点、RTX PRO 6000 四卡 EXL3 方案仍在 `mia-ailab/glm-5.3-flash` 模板中。昇腾需要额外补丁，海光/PPU/Apple 未纳入本模板。
