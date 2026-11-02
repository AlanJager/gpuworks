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
