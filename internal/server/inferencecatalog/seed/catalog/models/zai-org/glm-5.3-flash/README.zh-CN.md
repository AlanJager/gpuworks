# GLM-5.3-Flash

原始 FP8 权重约 328.3 GB。NVIDIA 配方参照 [SGLang 配方](https://docs.sglang.io/cookbook/autoregressive/GLM/GLM-5.3-Flash)：H200/H20 用 TileLang DSA + BF16 KV，B200/B300 用 TRT-LLM DSA + FP8 KV；两者都接受单节点 4 卡或 8 卡（TP = EP = 卡数）。上游只给出 8 卡命令，4 卡由本平台按 GB300 的 TP4 配方补充。MI355X × 4 使用 [vLLM ROCm 配方](https://recipes.vllm.ai/zai-org/GLM-5.3-Flash)，AMD 使用 ROCm 10 nightly 镜像。

H20 共用 Hopper 配方；默认参数下，96 GB 版本使用 8 卡，4 卡会被显存准入拒绝；141 GB 版本可评估 4/8 卡。准入使用节点上报的实际显存。以上指 SGLang 档；H20 的 SGLang 档尚未完成真机推理验证。

## vLLM 档（Hopper × 8，TP8）

vLLM 是本平台唯一支持 sleep/wake 的引擎，所以 Hopper 另有两个单节点 8 卡 TP8 的 vLLM 档，参数取自 [vLLM 配方](https://recipes.vllm.ai/zai-org/GLM-5.3-Flash)（关闭 FlashInfer autotune、`glm47` 工具与推理解析器、`VLLM_ENGINE_READY_TIMEOUT_S=3600`），并按 vLLM 0.30.0 的参数改写：配方里的 `--tokenizer-mode glm5_next` 在 0.30.0 中不存在（可选值不含它），不加；`--disable-flashinfer-autotune` 在 0.30.0 中写作 `--no-enable-flashinfer-autotune`。镜像用平台自带的 `gpuworks-runner:cuda13.0-vllm0.30.0-r2`（vLLM 0.30.0），不用上游 nightly：该镜像已注册 `Glm5NextForConditionalGeneration`，并在 8 × H20 141 GB 上跑通。两档都固定 8 卡，其他卡数会被拒绝；FP8 权重与上文 SGLang 档是同一份。

| 档位 | 适用显卡 | 上下文 |
| --- | --- | --- |
| `fp8-vllm-nvidia-hopper-141g-tp8` | H200、H20 141 GB（单卡 ≥ 120 GiB） | 模型原生 1M，不写 `--max-model-len` |
| `fp8-vllm-nvidia-hopper-80-96g-tp8` | H100 80 GB、H20 96 GB | 限制为 256K（`--max-model-len 262144`） |

每卡显存（TP8，`gpu_memory_utilization` 取部署默认 0.9；权重 38.96 GiB/卡为实测，其余列由显卡容量按 0.9 推算，不是实测）：

| 显卡 | 单卡容量 | 0.9 预算 | 权重 | 预算扣除权重后（激活、CUDA graph、KV） | 平台准入要求 |
| --- | --- | --- | --- | --- | --- |
| H200 / H20 141 GB | 约 140 GiB | 约 126 GiB | 38.96 GiB | 约 87 GiB | 约 43 GiB |
| H20 96 GB | 约 95.6 GiB | 约 86 GiB | 38.96 GiB | 约 47 GiB | 约 43 GiB |
| H100 80 GB | 约 79.6 GiB | 约 71.6 GiB | 38.96 GiB | 约 33 GiB | 约 43 GiB |

KV 占用很小：config 里 45 层只有 11 层（`full_attn_layers`）保存逐 token 的 KV，其余 34 层是线性注意力，状态大小与长度无关。因此 80 GB 档的瓶颈是激活与 CUDA graph 余量，而不是 KV；限制到 256K 是为了给这部分留足余量，不是 KV 放不下。H100 80 GB 的余量最小，若启动期 OOM 先降 `gpu_memory_utilization`。

没有启用的上游可选项：MTP 投机解码（`--speculative-config`）、FP8 KV（`--kv-cache-dtype fp8`）和 `VLLM_USE_RUST_FRONTEND`（Rust 前端是否带 sleep 开发接口未核实），需要时在部署表单的额外参数里加。

## 实测（vLLM，H20 141 GB × 8，TP8，2026-10-31）

Docker 运行时、`gpuworks-runner:cuda13.0-vllm0.30.0-r2`、模型放在 NFS（约 830 MB/s）、开启 sleep 模式。对话输出正确。权重加载 501 s；冷启动到就绪约 17 分钟，其中包含首次启动时的 TileLang 内核编译。Sleep/wake（L1，引擎自报）：sleep 1.9–2.1 s，wake 2.9–3.8 s。L2 唤醒需要重新加载权重，超过 10 分钟，不实用。H200 与 H100/H20 96 GB 档只做了目录与准入的离线检查，没有在真机上运行；上表的余量也是推算值。

昇腾档按 vLLM-Ascend v0.30.0rc2 的 A3 配方：单个 16 die 的 910C 节点、
TP16/EP16、W8A8 权重、INT8 KV cache 和 1M 上下文。模板固定完整 ModelScope 快照。
同一镜像的 MTP 和 KDA decode graph 还有未合并的上游修复，因此主模板默认关闭
speculative decoding 与 graph compilation，以 eager 模式提供基线。

2026-10-23，该档在 910C × 16 的原生 Docker 环境使用镜像原生行为启动，16/16 rank
完成初始化，平台 health、`/v1/models`（1M 上下文）和真实 chat 均通过。Docker 不注入
runveil/PRoot 兼容脚本。此结果不代表长上下文、并发、多模态或性能已经验收。

runveil/PRoot 路径尚未通过：兼容修复可推进 rank 与 group 初始化，但当前环境仍出现
CANN Fill/Transpose tiling 注册失败或 PRoot 线程饥饿，不能记为真机可用。

## 实测（B200 × 4，TP4/EP4，2026-10-20）

Vast 容器节点、runveil 运行时、`gpuworks-runner:cuda13.0-sglang0.5.21-r2`、EAGLE MTP 5/1/6。对话、思考、工具调用与图像输入均通过。每个请求约 1.5k 输入 token、固定 512 输出 token，在节点本机直连引擎测得（预热后）：

| 并发 | 总输出吞吐 | 单路解码 | 平均 TTFT | 平均 TPOT |
| --- | --- | --- | --- | --- |
| 1 | 241 token/s | 279 token/s | 0.29 s | 3.6 ms |
| 4 | 652 token/s | 193 token/s | 0.44 s | 5.2 ms |
| 16 | 1418 token/s | 97 token/s | 0.35 s | 10.3 ms |

runveil 下从启动到就绪约 13 分钟；首次遇到新的批形状时还要编译内核，并发 16 的前两轮 TTFT 达 5–10 秒，之后回落。H200 × 4 尚未实测。

已有 GB10 双节点、RTX PRO 6000 四卡 EXL3 方案仍在 `mia-ailab/glm-5.3-flash` 模板中。昇腾仅覆盖上述 910C Docker 场景，runveil 尚未通过；海光/PPU/Apple 未纳入本模板。

## 昇腾 910C 高性能配方（Graph / DFlash）

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

### 高性能 DFlash 实测范围

2026-10-25 在上述新镜像上实测：单机 16 个逻辑 NPU、TP2/DP8，32 路独立会话，每路输入 131072 token，预热 98304 token 历史，中英文和代码混合，temperature 0.6。单轮同步并发、无重试，每路实际输出 4096 token；通过 `X-data-parallel-rank` 固定预热与测量请求的 DP。

| 每路解码 TPS（最小 / 中位 / 最大） | 达到 40 TPS 的请求 | 首 token 延迟中位数 | 总输出 / 本轮墙钟时间 | 每路实际缓存 / 未缓存输入 |
| --- | --- | --- | --- | --- |
| **37.91 / 41.93 / 48.63** | 25 / 32 | 30.59 秒 | **945.80 token/s** | 95744 / 35328 token |

采用 `enable_fused_mc2=1`，不启用 MegaMoE，也不注入单独的 CANN 预编译流程。解码速度按首个 token bundle 之后的实际 token 增量计算；总输出吞吐含本轮 prefill 与等待，不含历史预热。所有 DP 的 Graph replay、32/32 请求与缓存口径、冷/部分命中/全命中已知答案均通过；32 次并发预热和 64 次多位置答案检查全部通过。输入 token 与旧高性能配置的混合负载逐一一致。

这是单轮结果，不表示 32 路都稳定达到 40 TPS；7 路低于此值。尚未对新镜像完成持续负载验收，也不能由这些定向答案检查推断完整业务准确率。下面原版 Graph 的输出长度为 1024，不能用两张表直接计算加速比例。

### 原版 Graph 实测范围

2026-10-25 在单台 910C 上验证以下两档。每个请求输入 131072 token，独立预热 98304 token 历史，中英文和代码混合；单轮同步并发、无重试，输出上限 1024 token，所有请求实际输出均为 1024 token。压测通过 `X-data-parallel-rank` 将每个会话的预热和后续请求固定到同一个 DP；实际业务也需要满足前缀回到原 DP 的条件才能复用这些缓存。

| 逻辑 NPU / 并发 | 每路解码 TPS（最小 / 中位 / 最大） | 首 token 延迟中位数 | 总输出 / 测试墙钟时间 | 每路实际缓存 token |
| --- | --- | --- | --- | --- |
| 16 / 32 | 25.35 / 25.73 / 26.03 | 29.00 秒 | 475.91 token/s | 97152 |
| 8 / 4 | 31.10 / 31.19 / 31.29 | 12.47 秒 | 90.43 token/s | 97920 |

每路解码 TPS 按首个 token bundle 之后的 token 数和解码时间计算；总输出吞吐包含本轮 prefill 和等待时间，不含预热历史的耗时。实际未缓存输入分别为 33920 和 33152 token。两档均确认全部 DP rank 的 Graph replay，冷缓存、部分命中、全部命中的已知答案检查通过；多位置事实检索分别通过 64/64 和 8/8 次答案检查。这是单轮验收，并非持续负载测试；尚未达到 32 路每路 40 TPS 的目标。

### 镜像与 graph

固定使用官方 `nightly-releases-v0.30.0-a3` 在 2026-10-24 构建的 ARM64 镜像，镜像摘要见 `model.yaml`。这是 release 分支 nightly，包含 CANN 9.2.0；宿主驱动和固件须与其匹配。模板使用 V1 model runner 和 `FULL_DECODE_ONLY`，没有 `--enforce-eager`。镜像已包含 GLM5Next graph 补齐请求的卷积状态越界修复。原版 Graph 配置显式关闭 DSA context parallel：在固定镜像的 TP2/DP8 配置中，开启它会使 128K 已知答案用例在冷缓存和缓存命中下均产生异常输出；关闭后的同输入对照通过。

原版 Graph 捕获大小必须按 TP 对齐，且不能超过每个 DP 的序列上限，因此两档都设置 `--max-num-seqs 4`。DFlash 的捕获大小为 `[6,12,24]`，包含每请求 5 个草稿 token 的验证空间。

官方单机指南使用 TP16/DP1，并给出 MTP graph 配置；本模板采用不同的 TP/DP 拓扑。原版配置不启用投机解码；高性能配置使用 Modal DFlash，而非官方指南中的 MTP。

- [官方 GLM-5.3-Flash 部署指南](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/GLM5.3-Flash.html)
- [官方版本兼容矩阵](https://docs.vllm.ai/projects/ascend/en/latest/community/versioning_policy.html)
- [graph 补齐请求修复](https://github.com/vllm-project/vllm-ascend/pull/18878)

模板使用 `callable` 启动方式传递 Ascend 专用 INT8 KV 参数，实际推理进程仍为 vLLM。提供 `/health`、OpenAI chat streaming 和 `/metrics`；vLLM 以部署的服务名（`${SERVED_MODEL_NAME}`）对外应答。

### DFlash 补丁与历史测量

高性能配置使用与原版 Graph 相同的官方镜像摘要，但启动时在容器可写层安装固定的 15 个模块。补丁包含 GLM 辅助隐藏状态、草稿权重对齐与因果性、混合 KV 布局、DSA 索引器和前缀检查点保留；保留镜像已有的卷积 Graph 补齐修复。每个原文件和替换文件都由 SHA-256 校验，未知镜像源码会拒绝启动。不能只修改镜像标签来升级。补丁源码与重建方法见仓库 `build/inference-templates/glm53-910c/README.md`。

历史旧镜像 DFlash 的代码审阅负载在 32 路、131072 输入、4096 输出下，逐请求解码最小 / 中位 / 最大为 **39.65 / 41.81 / 48.07 TPS**。中英文和代码混合、相同并发及输入输出长度的最终缓存保留配置为 **35.96 / 41.50 / 50.90 TPS**，21/32 路达到 40 TPS。两者是不同负载，不应宣称所有业务都能稳定达到每路 40 TPS。历史混合负载持续闭环测试还出现了 prefill 干扰，不能用单轮解码速度代替持续服务性能。
