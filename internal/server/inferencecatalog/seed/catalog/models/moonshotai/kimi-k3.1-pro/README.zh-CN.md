# Kimi-K3.1-Pro

Kimi-K3.1-Pro 沿用 Kimi-K3 的结构（Kimi Delta Attention、896 个路由专家每 token 激活 16 个、视觉编码器），并给 24 个 MLA 层加了 top-2048 索引器的稀疏注意力。权重中路由专家为 MXFP4（磁盘约 1.56 TB），其余为 BF16；上下文 1M token。

两个引擎都需要随模型发布的首发镜像，平台 runner 和更早的上游镜像加载不了：

- **SGLang**，来自 [SGLang cookbook](https://docs.sglang.io/cookbook/autoregressive/Moonshotai/Kimi-K3.1-Pro)：NVIDIA 用 `lmsysorg/sglang:dev-kimi-k31-pro`，MI350X / MI355X 用 `lmsysorg/sglang:dev-kimi-k31-pro-mi35x`。SGLang 固定用 FP8 KV cache。
- **vLLM**，来自 [vLLM recipe](https://recipes.vllm.ai/moonshotai/Kimi-K3.1-Pro)：NVIDIA 用 `vllm/vllm-openai:kimi-k31`（CUDA 13），AMD 用 `vllm/vllm-openai-rocm:kimi-k31`（ROCm 10），需要 vLLM 0.34.0 及以上；PPU 用厂商的 vLLM 0.32.0 构建 `egslingjun-registry.cn-wulanchabu.cr.aliyuncs.com/egslingjun-tmp/inference-xpu-pytorch:kimi-k3.1-pro-vllm0.32.0-cuda13.0-pytorch2.13.0-20261103`。

## 档位

| 档位 | 硬件 | 说明 |
| --- | --- | --- |
| `mxfp4-vllm-nvidia-b300-tp8` | 8 × B300 | recipe 默认 TP8，**已真机验证**（见下文） |
| `mxfp4-vllm-nvidia-b300-tep8` | 8 × B300 | recipe TEP：MegaMoE 专家并行，长 prompt 首 token 最快 |
| `mxfp4-vllm-nvidia-b300-tp8-dcp8` | 8 × B300 | recipe decode 上下文并行：KV token 容量约为 TP8 的 7 倍，decode 慢 7–25% |
| `mxfp4-vllm-nvidia-gb300-tp8-multinode` | 2 个 GB300 tray（2 × 4） | recipe 默认 TP8，跨两个 tray |
| `mxfp4-vllm-amd-mi355x-tp8` | 8 × MI355X | recipe TP8，带 AMD AITER 环境变量 |
| `mxfp4-vllm-ppu-810e-tp32-ep-multinode` | 2 × 16 PPU 810E（TP32 · EP32） | recipe 多机 TEP，MoE 走 W4A16 的 `ppu_deep_gemm_w4a16` |
| `mxfp4-vllm-ppu-m890-tp8-dp-multinode` | 4 × 8 PPU M890（TP8 × DP4 · EP32） | recipe 多机 TEP，每台节点一个 TP8 数据并行副本，DeepEP 低延迟 all-to-all |
| `mxfp4-sglang-nvidia-b200-tp-dcp-multinode` | 2 × 8 B200（TP16 · DCP16） | cookbook 的 Balanced 命令；DCP 跟随部署的 TP |
| `mxfp4-sglang-nvidia-b300-tp8` | 8 × B300 | cookbook 低延迟 TP8 |
| `mxfp4-sglang-nvidia-b300-tp8-dcp8` | 8 × B300 | cookbook 高并发 TP8 · DCP8 · EP2 |
| `mxfp4-sglang-amd-mi355x-tp8` | 8 × MI350X / MI355X | cookbook 低延迟 TP8，AMD monokernel，1–4 并发 |
| `mxfp4-sglang-amd-mi355x-tp8-dcp8` | 8 × MI350X / MI355X | cookbook 高吞吐 TP8 · DCP8，最多 48 并发 |

NVIDIA 与 AMD 的 vLLM 档位比 recipe 多了两个真机上发现必须的参数：`--max-model-len 262144`（1M token 的单个请求每卡要 16.92 GiB KV，275 GB 的 B300 放下 195 GiB 权重后不够）和 `--max-num-seqs 128`（每条解码序列占一个 Kimi Delta Attention 状态块，vLLM 默认的 1024 超过了能放下的 839 个，引擎拒绝启动）。卡更大时可在部署表单调高上下文；275 GB 的 B300 上 vLLM 估算上限约 876K token。

多机档位要求各节点共用一个参数网络，见平台文档的多机推理。TP 填总卡数（两台 B200 填 16，两台 810E 填 32，两个 GB300 tray 填 8）。M890 档位每台节点固定 8 张卡跑 TP8，每台节点一个数据并行副本：部署时选四台节点，TP 填 32，「数据并行」填 4（等于节点数），引擎按 TP8 × DP4 运行；不填数据并行时启动前就会因卡数不符被拒绝。

PPU 档位去掉了上游 recipe 的三项设置：`NCCL_SOCKET_IFNAME=eth0` 和 `NCCL_IB_HCA=fic2,vsolar` 是 recipe 所用主机的网口名，平台会按参数网络自行设置；平台参数字典不接受 `--max-model-len auto`，不写时 vLLM 使用完整的 1M 上下文，32 张 PPU 放得下它的 KV cache。

## 实测

`mxfp4-vllm-nvidia-b300-tp8`，8 × B300 SXM6（每卡 275 GB），权重放在共享存储上：

- 权重冷加载 189 s、页缓存热时 107 s，每卡 195.04 GiB；CUDA graph 53 s、占 5.71 GiB；KV 池每卡 33.51 GiB，共 1,972,777 token（262K 请求可并发 7.53 个）。首次启动另有约 23 分钟的 flashinfer MoE 自动调优，部署的启动超时请设为 3600 秒。
- 对话与 reasoning 解析（`reasoning_content`）正常；工具调用未测试。
- 平台评测，全部请求成功：随机 2048/512（服务端计数输入 3,180 token）在并发 64 时输出 1,473 tok/s、总吞吐 10,624 tok/s；Agentic 80K（13 轮，实测输入约 119K）并发 8 时输出 225 tok/s；Agentic 200K（实测输入约 203K）并发 4 时输出 137 tok/s、总吞吐 127,066 tok/s，后续轮次前缀缓存命中 98%。

Kimi 的 tokenizer 是 tiktoken（`tiktoken.model`），评测的自动 tokenizer 找不到 `tokenizer.json`；用 transformers 的 `TikTokenConverter` 转换后 token id 与原版一致，可作为评测 tokenizer 导入。

## 910C (A3) · W4A8

`w4a8-vllm-ascend-a3-multinode-16npu` 使用官方 `kimi-k3.1-pro-a3` 镜像与 W4A8 权重（约 1.47 TB，显存约 1.34 TiB），采用单实例多机共置：节点内 TP16，每节点一个流水线阶段，两节点为能装下权重的最少节点数，四节点对应上游 Prefill 的阶段数。缓存精度、块大小、解析器与 MLAPO 取自上游 Decode 角色；DCP / PCP 与交织大小属于分离角色，未纳入。共置模式不启用 Decode 专用的 `recompute_scheduler_enable`。

该配置已在两台各 16 个 910C die 的节点上完成 TP16 × PP2 验证：`--max-model-len 210000`、`--max-num-seqs 64`、`--gpu-memory-utilization 0.90`。随机 2048/512、Agentic 80K 与 Agentic 200K 三组共 836 个请求全部成功，另完成 93 个多长度、真实多轮、10K Agentic 与 200K 单请求。推理与工具调用需要 `--tokenizer-mode`、两个解析器与 `--enable-auto-tool-choice` 同时配置，模板已一并给出。昇腾多机跑在加速器 RDMA 或 HCCS 网络上，部署需要一个覆盖全部参与节点的此类网络。950DT 未给档位——单节点 8 卡装不下这份权重，也没有 950DT 硬件。

## 不支持

- **Hopper（H100 / H200）**：现有镜像下，SGLang 加载权重时 OOM（sm90 上路由专家没有按 MXFP4 加载），vLLM 卡在 `attn_res` 内核的 Triton 回退实现上（该内核只在 Blackwell 有原生实现）。上游也把这两条命令标为未验证；等镜像能在 Hopper 上运行后再放回档位。
- 未包含：投机解码（SGLang DFLASH 用 `RadixArk/Kimi-K3.1-Dflash2-Preview`、vLLM DSpark 用 `Inferact/Kimi-K3.1-Pro-DSpark-preview`）、HiCache / 主机 KV 卸载、PD 分离、MI355X AgentX 预设，PPU 的 SGLang PD 配方，以及 Vera Rubin、TPU 配方。

只有 `mxfp4-vllm-nvidia-b300-tp8` 通过了本平台真机验收，其余档位取自上游、尚未在本平台验证。ModelScope 仓库目前为空，权重只从 Hugging Face 拉取；访问不了 Hugging Face 的节点需要从其他节点同步或从共享存储读取权重。
