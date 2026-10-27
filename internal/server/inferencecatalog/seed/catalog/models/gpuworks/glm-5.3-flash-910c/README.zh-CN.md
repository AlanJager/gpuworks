# GLM-5.3-Flash · Ascend 910C

单机文本推理模板，使用固定摘要的官方 vLLM Ascend 镜像、W8A8 权重与 INT8 KV cache，开启 decode graph。提供两种启动方式：

- **高性能 DFlash，16 个逻辑 NPU**：`w8a8-vllm-910c-dflash5-16npu`，TP2/DP8，5 个草稿 token。模板会校验并安装随附源码补丁，另外下载固定版本的 Modal DFlash 草稿模型；用于优先考虑解码吞吐的场景。
- **原版 Graph，16 或 8 个逻辑 NPU**：名称包含 `stock-graph`，直接启动 `vllm serve`，不安装自定义补丁，不下载草稿模型。

下面的两档设备拓扑都支持原版 Graph；DFlash 高性能配置使用 16 设备拓扑。

| 逻辑 NPU | TP / DP | 每个 DP 的最大序列数 | 用途 |
| --- | --- | --- | --- |
| 16 × 至少 64 GiB | TP2 / DP8 | 4 | DFlash 高性能 / 原版 Graph |
| 8 × 至少 64 GiB | TP4 / DP2 | 4 | 使用较少设备 |

这里按运行时可见的逻辑设备计数。8 张双芯片物理卡通常对应 16 个逻辑 NPU。4 × 64 GiB 的总显存不足以容纳约 309 GiB 的 W8A8 权重，因此不提供 4 设备配置。

上下文上限为 139264 token，覆盖 128K 输入及输出余量；prefill chunk 为 2048 token，长请求阈值为 512 token。缓存命中取决于状态缓存对齐及实际保留情况，不能仅凭预热了 96K 历史就假定每次只需计算 32K 新输入。各 DP 的缓存相互独立，历史复用还要求请求回到持有前缀的 DP。8 设备配置也不表示支持 32 路独立长历史驻留。

## 高性能 DFlash 实测范围

2026-10-25 在上述新镜像上实测：单机 16 个逻辑 NPU、TP2/DP8，32 路独立会话，每路输入 131072 token，预热 98304 token 历史，中英文和代码混合，temperature 0.6。单轮同步并发、无重试，每路实际输出 4096 token；通过 `X-data-parallel-rank` 固定预热与测量请求的 DP。

| 每路解码 TPS（最小 / 中位 / 最大） | 达到 40 TPS 的请求 | 首 token 延迟中位数 | 总输出 / 本轮墙钟时间 | 每路实际缓存 / 未缓存输入 |
| --- | --- | --- | --- | --- |
| **37.91 / 41.93 / 48.63** | 25 / 32 | 30.59 秒 | **945.80 token/s** | 95744 / 35328 token |

采用 `enable_fused_mc2=1`，不启用 MegaMoE，也不注入单独的 CANN 预编译流程。解码速度按首个 token bundle 之后的实际 token 增量计算；总输出吞吐含本轮 prefill 与等待，不含历史预热。所有 DP 的 Graph replay、32/32 请求与缓存口径、冷/部分命中/全命中已知答案均通过；32 次并发预热和 64 次多位置答案检查全部通过。输入 token 与旧高性能配置的混合负载逐一一致。

这是单轮结果，不表示 32 路都稳定达到 40 TPS；7 路低于此值。尚未对新镜像完成持续负载验收，也不能由这些定向答案检查推断完整业务准确率。下面原版 Graph 的输出长度为 1024，不能用两张表直接计算加速比例。

## 原版 Graph 实测范围

2026-10-25 在单台 910C 上验证以下两档。每个请求输入 131072 token，独立预热 98304 token 历史，中英文和代码混合；单轮同步并发、无重试，输出上限 1024 token，所有请求实际输出均为 1024 token。压测通过 `X-data-parallel-rank` 将每个会话的预热和后续请求固定到同一个 DP；实际业务也需要满足前缀回到原 DP 的条件才能复用这些缓存。

| 逻辑 NPU / 并发 | 每路解码 TPS（最小 / 中位 / 最大） | 首 token 延迟中位数 | 总输出 / 测试墙钟时间 | 每路实际缓存 token |
| --- | --- | --- | --- | --- |
| 16 / 32 | 25.35 / 25.73 / 26.03 | 29.00 秒 | 475.91 token/s | 97152 |
| 8 / 4 | 31.10 / 31.19 / 31.29 | 12.47 秒 | 90.43 token/s | 97920 |

每路解码 TPS 按首个 token bundle 之后的 token 数和解码时间计算；总输出吞吐包含本轮 prefill 和等待时间，不含预热历史的耗时。实际未缓存输入分别为 33920 和 33152 token。两档均确认全部 DP rank 的 Graph replay，冷缓存、部分命中、全部命中的已知答案检查通过；多位置事实检索分别通过 64/64 和 8/8 次答案检查。这是单轮验收，并非持续负载测试；尚未达到 32 路每路 40 TPS 的目标。

## 镜像与 graph

固定使用官方 `nightly-releases-v0.30.0-a3` 在 2026-10-24 构建的 ARM64 镜像，镜像摘要见 `model.yaml`。这是 release 分支 nightly，包含 CANN 9.2.0；宿主驱动和固件须与其匹配。模板使用 V1 model runner 和 `FULL_DECODE_ONLY`，没有 `--enforce-eager`。镜像已包含 GLM5Next graph 补齐请求的卷积状态越界修复。原版 Graph 配置显式关闭 DSA context parallel：在固定镜像的 TP2/DP8 配置中，开启它会使 128K 已知答案用例在冷缓存和缓存命中下均产生异常输出；关闭后的同输入对照通过。

原版 Graph 捕获大小必须按 TP 对齐，且不能超过每个 DP 的序列上限，因此两档都设置 `--max-num-seqs 4`。DFlash 的捕获大小为 `[6,12,24]`，包含每请求 5 个草稿 token 的验证空间。

官方单机指南使用 TP16/DP1，并给出 MTP graph 配置；本模板采用不同的 TP/DP 拓扑。原版配置不启用投机解码；高性能配置使用 Modal DFlash，而非官方指南中的 MTP。

- [官方 GLM-5.3-Flash 部署指南](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/GLM5.3-Flash.html)
- [官方版本兼容矩阵](https://docs.vllm.ai/projects/ascend/en/latest/community/versioning_policy.html)
- [graph 补齐请求修复](https://github.com/vllm-project/vllm-ascend/pull/18878)

模板使用 `callable` 启动方式传递 Ascend 专用 INT8 KV 参数，实际推理进程仍为 vLLM。提供 `/health`、OpenAI chat streaming 和 `/metrics`；vLLM 以部署的服务名（`${SERVED_MODEL_NAME}`）对外应答。

## DFlash 补丁与历史测量

高性能配置使用与原版 Graph 相同的官方镜像摘要，但启动时在容器可写层安装固定的 15 个模块。补丁包含 GLM 辅助隐藏状态、草稿权重对齐与因果性、混合 KV 布局、DSA 索引器和前缀检查点保留；保留镜像已有的卷积 Graph 补齐修复。每个原文件和替换文件都由 SHA-256 校验，未知镜像源码会拒绝启动。不能只修改镜像标签来升级。补丁源码与重建方法见仓库 `build/inference-templates/glm53-910c/README.md`。

历史旧镜像 DFlash 的代码审阅负载在 32 路、131072 输入、4096 输出下，逐请求解码最小 / 中位 / 最大为 **39.65 / 41.81 / 48.07 TPS**。中英文和代码混合、相同并发及输入输出长度的最终缓存保留配置为 **35.96 / 41.50 / 50.90 TPS**，21/32 路达到 40 TPS。两者是不同负载，不应宣称所有业务都能稳定达到每路 40 TPS。历史混合负载持续闭环测试还出现了 prefill 干扰，不能用单轮解码速度代替持续服务性能。
