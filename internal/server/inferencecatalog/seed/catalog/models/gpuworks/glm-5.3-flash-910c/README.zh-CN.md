# GLM-5.3-Flash · Ascend 910C · Graph

单机文本推理模板，使用官方 vLLM Ascend 镜像、W8A8 权重与 INT8 KV cache，开启 decode graph。直接启动 `vllm serve`，不安装自定义补丁，不下载草稿模型。

| 逻辑 NPU | TP / DP | 每个 DP 的最大序列数 | 用途 |
| --- | --- | --- | --- |
| 16 × 至少 64 GiB | TP2 / DP8 | 4 | 优先匹配的吞吐配置 |
| 8 × 至少 64 GiB | TP4 / DP2 | 4 | 使用较少设备 |

这里按运行时可见的逻辑设备计数。8 张双芯片物理卡通常对应 16 个逻辑 NPU。4 × 64 GiB 的总显存不足以容纳约 309 GiB 的 W8A8 权重，因此不提供 4 设备配置。

上下文上限为 139264 token，覆盖 128K 输入及输出余量；prefill chunk 为 2048 token，长请求阈值为 512 token。缓存命中取决于状态缓存对齐及实际保留情况，不能仅凭预热了 96K 历史就假定每次只需计算 32K 新输入。各 DP 的缓存相互独立，历史复用还要求请求回到持有前缀的 DP。8 设备配置也不表示支持 32 路独立长历史驻留。

## 实测范围

2026-10-25 在单台 910C 上验证以下两档。每个请求输入 131072 token，独立预热 98304 token 历史，中英文和代码混合；单轮同步并发、无重试，输出上限 1024 token，所有请求实际输出均为 1024 token。压测通过 `X-data-parallel-rank` 将每个会话的预热和后续请求固定到同一个 DP；实际业务也需要满足前缀回到原 DP 的条件才能复用这些缓存。

| 逻辑 NPU / 并发 | 每路解码 TPS（最小 / 中位 / 最大） | 首 token 延迟中位数 | 总输出 / 测试墙钟时间 | 每路实际缓存 token |
| --- | --- | --- | --- | --- |
| 16 / 32 | 25.35 / 25.73 / 26.03 | 29.00 秒 | 475.91 token/s | 97152 |
| 8 / 4 | 31.10 / 31.19 / 31.29 | 12.47 秒 | 90.43 token/s | 97920 |

每路解码 TPS 按首个 token bundle 之后的 token 数和解码时间计算；总输出吞吐包含本轮 prefill 和等待时间，不含预热历史的耗时。实际未缓存输入分别为 33920 和 33152 token。两档均确认全部 DP rank 的 Graph replay，冷缓存、部分命中、全部命中的已知答案检查通过；多位置事实检索分别通过 64/64 和 8/8 次答案检查。这是单轮验收，并非持续负载测试；尚未达到 32 路每路 40 TPS 的目标。

## 镜像与 graph

固定使用官方 `nightly-releases-v0.30.0-a3` 在 2026-10-24 构建的 ARM64 镜像，镜像摘要见 `model.yaml`。这是 release 分支 nightly，包含 CANN 9.2.0；宿主驱动和固件须与其匹配。模板使用 V1 model runner 和 `FULL_DECODE_ONLY`，没有 `--enforce-eager`。镜像已包含 GLM5Next graph 补齐请求的卷积状态越界修复。模板显式关闭 DSA context parallel：在固定镜像的 TP2/DP8 配置中，开启它会使 128K 已知答案用例在冷缓存和缓存命中下均产生异常输出；关闭后的同输入对照通过。

Graph 捕获大小必须按 TP 对齐，且不能超过每个 DP 的序列上限，因此两档都设置 `--max-num-seqs 4`。

官方单机指南使用 TP16/DP1，并给出 MTP graph 配置；本模板采用不同的 TP/DP 拓扑，且未开启 MTP 或 DFlash。过去自定义 DFlash 补丁版的性能数据不适用于本模板。

- [官方 GLM-5.3-Flash 部署指南](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/GLM5.3-Flash.html)
- [官方版本兼容矩阵](https://docs.vllm.ai/projects/ascend/en/latest/community/versioning_policy.html)
- [graph 补齐请求修复](https://github.com/vllm-project/vllm-ascend/pull/18878)

模板使用 `callable` 启动方式传递 Ascend 专用 INT8 KV 参数，实际推理进程仍为 vLLM。提供 `/health`、OpenAI chat streaming 和 `/metrics`；服务模型名为 `glm53-flash-910c-stock-graph`。
