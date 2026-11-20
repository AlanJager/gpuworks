# Qwen3.8-Flash-Next

[官方 SGLang 配方](https://docs.sglang.io/cookbook/autoregressive/Qwen/Qwen3.8-Flash-Next)。原始 FP8 权重约 185.5 GB；单节点 H200/H20 × 4 或 MI350X/MI355X × 8。使用配方专用镜像。NVIDIA 启用原生 MTP，AMD 使用 AITER 配方。

现有 Apple GGUF 和 NVIDIA Strata 模板保持独立；NVIDIA H200 × 4 配方已在本平台实测（见下），AMD 配方尚未实测。昇腾档位使用 vLLM-Ascend 官方实验性 W8A8-MTP 配方：固定权重 revision `c76aa96a…`，8 个 64 GiB 的 A3 逻辑设备，TP8。910B（A2）另有单机 4 卡档，见下文。不能使用本页 NVIDIA/AMD 的 FP8 模板代替。海光和 PPU 未核实可执行配方。

H20 的 96 GB 和 141 GB 版本共用 4 卡配方，资源准入使用节点上报的实际显存。以上指 SGLang 档；H20 的 SGLang 档尚未完成真机推理验证。

## vLLM 档（Hopper × 8，TP8）

vLLM 是本平台唯一支持 sleep/wake 的引擎，所以 H200、H20（96/141 GB）、H100 80 GB 另有一个单节点 8 卡 TP8 的 vLLM 档 `bf16-vllm-nvidia-hopper-tp8`，其他卡数会被拒绝。参数取自 [vLLM 配方](https://recipes.vllm.ai/Qwen/Qwen3.8-Flash-Next)（`--max-num-seqs 256`、`qwen3_coder` 工具解析器、`qwen3` 推理解析器、`VLLM_ENGINE_READY_TIMEOUT_S=1800`）。镜像用平台自带的 `gpuworks-runner:cuda13.0-vllm0.30.0-r4`（vLLM 0.30.0），不用上游 nightly：该镜像已注册 `Qwen4ExpForConditionalGeneration`，上游配方也写明稳定版 vLLM ≥ 0.30.0 即可。

**权重是 BF16 的 `Qwen/Qwen3.8-Flash-Next`，不是上文 SGLang/AMD/PPU 档用的 FP8**：仓库约 335 GiB（360.0 GB），TP8 下每卡约 42 GiB（实测）；上文的 185.5 GB 只指 FP8 仓库。这是真机跑通的组合（TP8，不开专家并行）。上游的 vLLM 配方用 FP8 权重，且 TP8 必须加 `--enable-expert-parallel`（FP8 的 128 宽量化块无法 8 路切分），这一组合未在本平台运行，所以没有提供。

必须带 `--disable-custom-all-reduce`：实测在 H20 上开启 sleep 模式时，不带它 FULL CUDA graph 捕获会在 `custom_all_reduce.cuh:164 'invalid argument'` 失败，加上后正常。没有测过关闭 sleep 的情况，所以不能说明未开 sleep 时是否需要。

每卡显存（TP8，`gpu_memory_utilization` 取部署默认 0.9；权重约 42 GiB/卡为实测，其余列由显卡容量按 0.9 推算，不是实测）：

| 显卡 | 单卡容量 | 0.9 预算 | 权重 | 预算扣除权重后（激活、CUDA graph、KV、线性注意力状态） | 平台准入要求 |
| --- | --- | --- | --- | --- | --- |
| H200 / H20 141 GB | 约 140 GiB | 约 126 GiB | 约 42 GiB | 约 84 GiB | 约 47 GiB |
| H20 96 GB | 约 95.6 GiB | 约 86 GiB | 约 42 GiB | 约 44 GiB | 约 47 GiB |
| H100 80 GB | 约 79.6 GiB | 约 71.6 GiB | 约 42 GiB | 约 30 GiB | 约 47 GiB |

上下文保持模型原生 256K，不设 `--max-model-len`。KV 占用很小：48 层里只有 12 层是全注意力，且只有 2 个 KV 头，其余 36 层是线性注意力，状态与长度无关，所以 80 GB 卡也不需要缩上下文；H100 的余量最小，启动期 OOM 时先降 `gpu_memory_utilization` 或 `--max-num-seqs`。上游配方还说 H100 上默认把 N-gram embedding 卸载到主机内存，这一点没有在本平台验证，H100 的主机内存占用未知，需要时可显式加 `--engram-config '{"cpu_offload":true}'`。

没有启用的上游可选项：MTP 投机解码、`--enable-prefix-caching`、FP8 KV 和 `--attention-config.indexer_kv_dtype fp8`（都未在 0.30.0 上验证），需要时在部署表单的额外参数里加。

## 实测（vLLM，H20 141 GB × 8，TP8，2026-10-31）

Docker 运行时、`gpuworks-runner:cuda13.0-vllm0.30.0-r4`、BF16 权重放在 NFS（约 830 MB/s）、开启 sleep 模式、带 `--disable-custom-all-reduce`。对话输出正确。权重在页缓存中时，启动到就绪约 3.5 分钟；冷读 NFS 会更久。Sleep/wake（L1，引擎自报）：sleep 1.7–1.9 s，wake 2.9–3.7 s。L2 唤醒需要重新加载权重，489 s，不实用。H100 80 GB、H20 96 GB 和 H200 只做了目录与准入的离线检查，没有在真机上运行；上表的余量是推算值。

## 实测（H200 × 4，TP4，2026-10-19）

Vast 容器节点、runveil 运行时。对话、思考、工具调用通过；单路 1500 token 输出约 272 token/s，TTFT 0.71 s（经网关），NEXTN 接受长度 1.7–3.95；启动到就绪约 9 分钟。未做多并发压测。

## 实测（910C × 8，TP8，2026-10-22）

自有 910C 节点、Docker 运行时，使用 vLLM-Ascend 的 `qwen3.8-next-a3` 镜像（本页按
digest 固定）。自动排布避开了 ACL 上下文检查失败的那张 die，用其余 8 张启动；部署
达到 running，`/health`、`/v1/models` 和 OpenAI 兼容 chat 均返回 200，回答内容正确。
未做吞吐与多并发压测。

## 昇腾 910B（A2）· 4 卡 · TP4

单节点 4 张、每张至少 64 GiB 的 910B，TP4 + 专家并行，vLLM-Ascend `v0.32.0rc1-openeuler`
官方镜像（按多架构 index digest 固定；南大 quay 镜像站和 gpuworks 在华为 SWR 上的公开副本提供同一 digest，
国内从 quay.io 拉取可能很慢）。共四档：

| 档位 | 权重（ModelScope，固定 revision） | 仓库大小 | 投机解码 |
| --- | --- | --- | --- |
| `int8-vllm-910b-tp4` | `Eco-Tech/Qwen3.8-Flash-Next-w8a8-mtp` `c76aa96a…` | 约 245 GB | 无 |
| `int8-vllm-910b-tp4-mtp` | 同上 | 约 245 GB | MTP，3 个草稿 token |
| `w4a8-vllm-910b-tp4` | `Eco-Tech/Qwen3.8-Flash-Next-w4a8` `d75537fd…` | 约 194 GB | 无 |
| `w4a8-vllm-910b-tp4-mtp` | 同上 | 约 194 GB | MTP，3 个草稿 token |

两个仓库都自带 BF16 的 MTP 层，MTP 档不另外下载草稿模型。参数：256K 上下文、并发上限 4、单轮批处理
8192 token、每卡固定 20 GiB KV 池（`--kv-cache-memory-bytes`，此时 vLLM 不按显存比例分 KV）、
前缀缓存（`--mamba-cache-mode align`、`--block-size 4096`）、每请求最多 32 张图、Decode-only ACLGraph、
关闭异步调度、`qwen3` 推理解析器和 `qwen3_xml` 工具解析器。MTP 档的 decode 图尺寸改为 4 的倍数
（每条序列 1 个目标 token + 3 个草稿 token），草稿层按官方 910C 配方走 eager。

**主机内存**：`--engram-config` 把 51B 参数的逐层嵌入表放在主机内存，且每个 rank 各映射一份
（`dp_shared_memory=false`），4 个 rank 约需 96 GiB 空闲内存，另加权重文件的页缓存。

实测（910B4 64 GiB × 4，ARM64，驱动 25.5.0，2026-11-19）：W4A8 不开 MTP，参数与本档一致（当时用手写命令启动）。
每卡权重 18.65 GiB，KV 池 20 GiB（约 143 万 token，256K 请求可并发 5.48 条），decode 图捕获 83 s，
从加载到就绪约 9.5 分钟，对话请求正常返回，单路解码约 25 token/s。W8A8 档用同一组参数也已在该机型上跑通（用户提供的运行记录）。

MTP 实测（910B2 64 GiB × 4，ARM64，`w4a8-vllm-910b-tp4-mtp`，2026-11-20）：
每卡权重 19.88 GiB、加载 172 s，`torch.compile` 91–106 s。对话与流式都正常，单路解码约 26 token/s
（300 token 的回答 11.4 s）。投机解码按 `/metrics` 的 `vllm:spec_decode_num_{draft,accepted}_tokens_total`
区间差值统计：普通文本接受率 47%–58%（每步出 2.4–2.8 个 token），高度可预测的内容（顺序数数）达 88%
（每步 3.6 个）。`int8-vllm-910b-tp4-mtp` 与本档只差权重与量化方式，未单独实测。

**首次请求较慢**：部分算子内核按实际张量形状在首个请求到达时即时编译，所以第一个请求明显慢于
之后的请求，上表的吞吐是编译缓存就绪后的稳态值。编译结果缓存在容器内，容器重建后首个请求会再付一次。
