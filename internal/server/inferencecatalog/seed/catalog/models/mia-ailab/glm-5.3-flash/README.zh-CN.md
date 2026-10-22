# GLM-5.3-Flash（TensorFold EXL3）

两套实测配方，都用 `callable` 引擎承载 TensorFold，模板自带的适配器把 GPUWorks 的启动事实（模型路径、端口、并行度、rank）转换成 TensorFold 的进程布局：

- **DGX Spark（GB10）×2**：两节点 TP2，每节点一个 rank，RoCE 互联，q4 dense、FP8 KV、DFlash2，1M 上下文、4 路并发、图像/视频输入。已通过确定性质量探针，包括约 691K 输入 token 的隐藏 needle；长上下文吞吐受 prefill 限制。
- **RTX PRO 6000 / 6000D ×4（单机 PCIe）**：一台主机 4 张卡 TP4，一个容器里 4 个 rank，NCCL 走 PCIe P2P、大块交换走 CUDA IPC 拷贝引擎。复刻 Aevonix 的生产配置：FP8 dense、FP8 KV、DFlash2 草稿、40 路并发、1M 上下文、图像/视频输入。KV 池上限 60 GiB/卡，84 GB 的 6000D 上由引擎自动缩到放得下的大小。

两个档位的服务行为一致：默认开启思考、推理强度 `medium`、接受图像与视频输入（权重自带视觉塔，由 `--vision` 启用；请求里的图片和视频用 data URL）。GLM-5.3 的对话模板只有 Low / High / Max 三档，TensorFold 把 `medium` 映射为 High：简单问题几乎不思考，难题才展开推理。

**首次启动较慢**：TensorFold 首启要编译 CUDA 内核，并由各 rank 轮流在 CPU 上转换权重后再装到卡上。在只能用 udocker 的节点（如 AutoDL 4×RTX 6000D）上实测首启 61 分钟，大部分时间耗在 udocker 的系统调用转发上；docker 节点会快很多。部署时请在「运行环境」把「启动超时」设为 7200 秒（集群默认 1800 秒不够）。

**6000D 实测**：1M 上下文放得下；40 路并发共享约 325 万 token 的 KV 池（96 GB 卡约 510 万）；单请求解码约 185 tok/s，8 路并发合计约 530 tok/s。模板自带在 6000D 上实测生成的 autotune launch table（逐位校验，其他卡型自动退回默认配置），本配方下端到端提升在误差内。

## DGX Spark（GB10）×2 档位的部署要求

- 必须选两台 GB10 节点，每节点一个 rank；单节点部署会因缺少 rank 0 地址而被拒绝。TensorFold 的 TP 取节点数（两节点即 TP2），部署表单里的 TP/PP 不影响它。
- 两台节点要在同一个「主机 RDMA」参数网络上（RoCE 口），网卡、HCA 与 GID 由参数网络下发，模板不写死节点网卡名。没有参数网络时部署评估会给出原因。
- 两个 rank 都要拿到完整权重：各自从所选来源下载约 176 GB，或在每台节点登记同一份自包含的快照目录。
- 实测（两台 GB10，ModelScope 权重）：首启约 6.5 分钟健康；4 路短请求合计约 100 tok/s，约 9.4 万 token 的输入 prefill 约 1,750 tok/s。

## 可配置参数

callable 档位没有参数字典。命令行参数在部署表单的「自定义参数」里追加：适配器会把它们传给每个 rank 的 `tensorfold serve`，同名参数以后出现的为准，所以能覆盖模板的默认值。环境变量在「运行环境」的环境变量里设置，同名时覆盖模板的值。

| 参数 | GB10 ×2 默认 | RTX PRO 6000 / 6000D ×4 默认 | 说明 |
|---|---|---|---|
| `--context N` | `1048576` | `1048576` | 单个请求的上下文窗口（输入加输出），最大 1048576 |
| `--parallel N` | `4` | `40` | 同时解码的请求数，可设 1–64，所有请求共享一个 KV 池 |
| `--max-tokens N` | `32768` | `32768` | 请求不传 `max_tokens` 时的默认输出上限 |
| `--thinking` / `--no-thinking` | 开 | 开 | 默认是否思考；单个请求可用 `chat_template_kwargs.enable_thinking` 覆盖 |
| `--reasoning-effort low\|medium\|high\|xhigh` | `medium` | `medium` | 开启思考时的推理强度；GLM-5.3 上 `medium` 即 High，`xhigh` 即 Max；单个请求可用 `reasoning_effort` 覆盖 |
| `--vision` | 开 | 开 | 接受图像与视频输入；再加 `--vision-urls` 可接受公网 HTTP(S) 图片地址（默认只收 data URL） |
| `--drafter /draft` | DFlash2 | DFlash2 | 草稿模型；`--drafter none` 加环境变量 `TF_GLM_MTP=1` 改用模型自带的 MTP，此时 `--parallel` 只能为 1 |
| `TF_GLM_CACHE_GIB` | — | `60` | 每卡 KV 池的上限（GiB），引擎会按权重和预留之后的剩余显存自动缩小 |
| `TENSORFOLD_MEMORY_RESERVE_GIB` | — | `7.5` | 每卡留空的显存（GiB），不小于 2 |
| `TF_GLM_DENSE` | `q4` | `fp8` | dense 层精度：`q4` 更快、保真度更低（与 BF16 首 token 一致率 73%，`fp8` 为 86%），`bf16` 显存需求最大 |
| `TF_GLM_KV` | `fp8` | `fp8` | KV 精度；`bf16` 的 KV 池减半 |

**上下文怎么算、受什么限制**（下面的实测数字来自 RTX PRO 6000 / 6000D 档位）：
- 单请求上限是 1048576 token。
- 所有请求共享一个 KV 池，池子大小等于 `TF_GLM_CACHE_GIB` 与「每卡显存 − 权重 − 预留」二者中较小的值，引擎会自动收缩，不会因为池子放不下而 OOM。实测 96 GB 卡约 510 万 token，84 GB 的 6000D 约 325 万 token。
- 如果显式指定的 `--context` 连一个请求都放不下，启动时会直接报错，并给出能放下的最大值。
- 请求里的 `max_tokens` 决定单次输出上限，不传时用 `--max-tokens`；单次输出总是不超过「上下文 − 输入长度」。

**卡数与显存**：
- RTX PRO 6000 / 6000D 档位固定为一台主机上的 4 张卡、TP4，每卡至少 80 GiB。卡数和显存由 placement resolver 校验，不满足时部署评估会直接拒绝。

DFlash2 草稿权重是 CC BY-NC-ND 4.0（非商用）。
