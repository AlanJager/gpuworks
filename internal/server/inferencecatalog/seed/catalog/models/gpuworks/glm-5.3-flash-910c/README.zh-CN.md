# GLM-5.3-Flash · Ascend 910C · Graph

单机文本推理模板，使用官方 vLLM Ascend 镜像、W8A8 权重与 INT8 KV cache，开启 decode graph。直接启动 `vllm serve`，不安装自定义补丁，不下载草稿模型。

| 逻辑 NPU | TP / DP | 每个 DP 的最大序列数 | 用途 |
| --- | --- | --- | --- |
| 16 × 至少 64 GiB | TP2 / DP8 | 4 | 优先匹配的吞吐配置 |
| 8 × 至少 64 GiB | TP4 / DP2 | 2 | 使用较少设备 |

这里按运行时可见的逻辑设备计数。8 张双芯片物理卡通常对应 16 个逻辑 NPU。4 × 64 GiB 的总显存不足以容纳约 309 GiB 的 W8A8 权重，因此不提供 4 设备配置。

上下文上限为 139264 token，覆盖 128K 输入及输出余量；prefill chunk 为 2048 token，长请求阈值为 512 token。缓存命中取决于状态缓存对齐及实际保留情况，不能仅凭预热了 96K 历史就假定每次只需计算 32K 新输入。各 DP 的缓存相互独立，历史复用还要求请求回到持有前缀的 DP。8 设备配置也不表示支持 32 路独立长历史驻留。

## 镜像与 graph

固定使用官方 `nightly-releases-v0.30.0-a3` 在 2026-10-24 构建的 ARM64 镜像，镜像摘要见 `model.yaml`。这是 release 分支 nightly，包含 CANN 9.2.0；宿主驱动和固件须与其匹配。模板使用 V1 model runner 和 `FULL_DECODE_ONLY`，没有 `--enforce-eager`。镜像已包含 GLM5Next graph 补齐请求的卷积状态越界修复。

官方单机指南使用 TP16/DP1，并给出 MTP graph 配置；本模板采用不同的 TP/DP 拓扑，且未开启 MTP 或 DFlash。过去自定义 DFlash 补丁版的性能数据不适用于本模板。

- [官方 GLM-5.3-Flash 部署指南](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/GLM5.3-Flash.html)
- [官方版本兼容矩阵](https://docs.vllm.ai/projects/ascend/en/latest/community/versioning_policy.html)
- [graph 补齐请求修复](https://github.com/vllm-project/vllm-ascend/pull/18878)

模板使用 `callable` 启动方式传递 Ascend 专用 INT8 KV 参数，实际推理进程仍为 vLLM。提供 `/health`、OpenAI chat streaming 和 `/metrics`；服务模型名为 `glm53-flash-910c-stock-graph`。
