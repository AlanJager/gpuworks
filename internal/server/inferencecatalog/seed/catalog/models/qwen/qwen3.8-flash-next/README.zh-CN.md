# Qwen3.8-Flash-Next

[官方 SGLang 配方](https://docs.sglang.io/cookbook/autoregressive/Qwen/Qwen3.8-Flash-Next)。原始 FP8 权重约 185.5 GB；单节点 H200/H20 × 4 或 MI350X/MI355X × 8。使用配方专用镜像。NVIDIA 启用原生 MTP，AMD 使用 AITER 配方。

现有 Apple GGUF 和 NVIDIA Strata 模板保持独立；NVIDIA H200 × 4 配方已在本平台实测（见下），AMD 配方尚未实测。昇腾档位使用 vLLM-Ascend 官方实验性 W8A8-MTP 配方：固定权重 revision `c76aa96a…`，8 个 64 GiB 的 A3 逻辑设备，TP8。不能使用本页 NVIDIA/AMD 的 FP8 模板代替。海光和 PPU 未核实可执行配方。

H20 的 96 GB 和 141 GB 版本共用 4 卡配方，资源准入使用节点上报的实际显存。以上指 SGLang 档；H20 的 SGLang 档尚未完成真机推理验证。

## vLLM 档（Hopper × 8，TP8）

vLLM 是本平台唯一支持 sleep/wake 的引擎，所以 H200、H20（96/141 GB）、H100 80 GB 另有一个单节点 8 卡 TP8 的 vLLM 档 `bf16-vllm-nvidia-hopper-tp8`，其他卡数会被拒绝。参数取自 [vLLM 配方](https://recipes.vllm.ai/Qwen/Qwen3.8-Flash-Next)（`--max-num-seqs 256`、`qwen3_coder` 工具解析器、`qwen3` 推理解析器、`VLLM_ENGINE_READY_TIMEOUT_S=1800`）。镜像用平台自带的 `gpuworks-runner:cuda13.0-vllm0.30.0-r2`（vLLM 0.30.0），不用上游 nightly：该镜像已注册 `Qwen4ExpForConditionalGeneration`，上游配方也写明稳定版 vLLM ≥ 0.30.0 即可。

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

Docker 运行时、`gpuworks-runner:cuda13.0-vllm0.30.0-r2`、BF16 权重放在 NFS（约 830 MB/s）、开启 sleep 模式、带 `--disable-custom-all-reduce`。对话输出正确。权重在页缓存中时，启动到就绪约 3.5 分钟；冷读 NFS 会更久。Sleep/wake（L1，引擎自报）：sleep 1.7–1.9 s，wake 2.9–3.7 s。L2 唤醒需要重新加载权重，489 s，不实用。H100 80 GB、H20 96 GB 和 H200 只做了目录与准入的离线检查，没有在真机上运行；上表的余量是推算值。

## 实测（H200 × 4，TP4，2026-10-19）

Vast 容器节点、runveil 运行时。对话、思考、工具调用通过；单路 1500 token 输出约 272 token/s，TTFT 0.71 s（经网关），NEXTN 接受长度 1.7–3.95；启动到就绪约 9 分钟。未做多并发压测。

## 实测（910C × 8，TP8，2026-10-22）

自有 910C 节点、Docker 运行时，使用 vLLM-Ascend 的 `qwen3.8-next-a3` 镜像（本页按
digest 固定）。自动排布避开了 ACL 上下文检查失败的那张 die，用其余 8 张启动；部署
达到 running，`/health`、`/v1/models` 和 OpenAI 兼容 chat 均返回 200，回答内容正确。
未做吞吐与多并发压测。
