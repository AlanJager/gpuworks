# SenseNova-U1.6-8B-MoT

SenseNova-U1.6-8B-MoT 是商汤 NEO-unify 架构（Qwen3-8B 骨干 + 生成塔 + 像素头，`neo_chat` 模型类型）的文生图与图像编辑模型：BF16 权重 35.07 GB。GPUWorks 的 Playground 可以直接文生图和单图编辑，`/v1/images/generations` 与 `/v1/images/edits` 两个接口都经平台网关提供。

## 权重

两个档位用不同的权重仓库：

- **BF16 档**用原版权重（35.07 GB，`weight_bytes` 35065860072）：
  - Hugging Face `sensenova/SenseNova-U1.6-8B-MoT`，revision `8f582699bac7f1e5362df2a776397d0e1a963b6c`。
  - ModelScope `SenseNova/SenseNova-U1.6-8B-MoT`，revision `c03718b305258bbdc3994e132158ddfbda842268`。
  - 两个来源的文件按 sha256 一致（8 个 safetensors 分片 + tokenizer/config）。
- **FP8 档**用预量化的 FP8 W8A8（per-tensor）checkpoint（18.86 GB，`weight_bytes` 18864507420），由上面的原版 BF16 离线量化而来（基于原版 revision `8f58269`）：
  - Hugging Face `gpuworks/SenseNova-U1.6-8B-MoT-FP8`，revision `380e219037baa980ce598fc1084b9a58c5fb1294`。
  - ModelScope `gpuworks/SenseNova-U1.6-8B-MoT-FP8`，revision `80dc710238d2d7198f896cea4e17409534cf7646`。
  - 仓库的 `config.json` 自带 `quantization_config`，vLLM-Omni 直接识别，启动参数不需要 `--quantization fp8`。

## 镜像与引擎

两个档位都用 GPUWorks 的 vLLM-Omni 0.34 镜像（`gpuworks/gpuworks-runner:cuda13.0-vllm-omni0.34.0-r1`，vLLM 0.34.0 + vLLM-Omni 0.34.0），`--omni` 启动；不声明 offload 档位：vLLM-Omni 对 SenseNova 架构（`SenseNovaU1ForCausalLM`）报 `No _layerwise_offload_blocks_attrs defined`（0.32 实测，0.34 同样没有），上游的逐层 offload 支持表里没有它。

## 档位

| 档位 | GPU | 适用 | 单卡显存峰值 |
|---|---:|---|---:|
| FP8 · 单卡 | 1 | Ampere 及以上，单卡 ≥ 22 GiB | 21.1–22.0 GiB |
| BF16 · 1–2 卡 | 1 或 2 | 单卡 ≥ 40 GiB，或双卡每卡 ≥ 22 GiB | 单卡 36.0 GiB；双卡 17–18.7 GiB/卡 |

**FP8**（`--omni`，权重是预量化的 FP8 checkpoint）：Ada 及以上走 CUTLASS W8A8，Ampere（SM86，如 RTX 3090）没有 FP8 Tensor Core，退回 Marlin W8A16（FP8 权重、BF16/FP16 激活）。两种路径都在真实硬件上用本模板的镜像和这个 checkpoint 验证过（RTX 4090、RTX 3090）。预量化 checkpoint 与 vLLM-Omni 的在线 FP8（`--quantization fp8` 加载 BF16 原版）输出逐像素一致，区别只在加载：磁盘与下载量 18.9 GB（原版 35.1 GB），加载时主机内存峰值约 2.8 GiB（在线量化 BF16 原版约 12.3 GiB，均在 vast 的 4090 上测得）。

**BF16**（`--omni`，不量化）单卡需要装下完整 35 GB 权重，两卡时平台按放置到的 GPU 数注入 `--tensor-parallel-size`，模型按张量并行切分到两卡。两种布局都能放下时优先选单卡。

## 实测性能

每行 50 步、并发 1、端到端中位数（含网络与网关开销，除非标注"直连引擎"）。"镜像 / 引擎" 一列写明实际跑的镜像与 vLLM-Omni 版本：**本模板镜像 r1** 指 `cuda13.0-vllm-omni0.34.0-r1`；其余行是更早的测量，用的是上游 `vllm/vllm-omni:v0.34.0` 或旧的 0.32.0 镜像，仅供对照。

| 档位 | 卡与来源 | 镜像 / 引擎 | 1024×1024 | 2048×2048 | 单卡显存峰值 |
|---|---|---|---:|---:|---:|
| FP8 · 单卡 | RTX 4090 24GB（vast 租机，直连引擎，预量化 FP8） | 本模板镜像 r1 / 0.34.0 | 8.3 s | 37.2 s | 21.1 GiB |
| FP8 · 单卡 | RTX 4090 24GB（同一台，直连引擎，在线 FP8 自 BF16 原版） | 本模板镜像 r1 / 0.34.0 | 8.3 s | 37.3 s | 21.7 GiB |
| FP8 · 单卡 | RTX 4090 24GB（vast 租机，另一台，在线 FP8） | 上游 vllm-omni:v0.34.0 / 0.34.0 | 9.2–10.2 s | 42.0 s | 21.68 GiB |
| FP8 · 单卡 | RTX 4090 48GB（本模板旧镜像，在线 FP8） | 旧 0.32.0-r2 / 0.32.0 | 8.7–9.1 s | 38.1 s | 21.74 GiB |
| FP8 · 单卡 | RTX 3090 24GB（vast 租机，直连引擎，预量化 FP8） | 本模板镜像 r1 / 0.34.0 | 27.5 s | 119.4 s | 21.1 GiB |
| FP8 · 单卡 | RTX 3090 24GB（同一台，直连引擎，在线 FP8 自 BF16 原版） | 本模板镜像 r1 / 0.34.0 | 27.5 s | 119.4 s | 22.1 GiB |
| FP8 · 单卡 | RTX 3090 24GB（vast 租机，另一台，在线 FP8） | 上游 vllm-omni:v0.34.0 / 0.34.0 | 27.4–30.0 s | 120.3 s | 21.99 GiB |
| BF16 · 单卡 | RTX 4090 48GB（本模板旧镜像） | 旧 0.32.0-r2 / 0.32.0 | 12.5–13.3 s | 55.0 s | 36.04 GiB |
| BF16 · 双卡 TP2 | 2 × RTX 3090 24GB（旧镜像，GPUWorks 平台部署，经网关实测） | 旧 0.32.0-r2 / 0.32.0 | 24.9 s | 100.5 s | 18.66 GiB/卡 |
| BF16 · 双卡 TP2 | 2 × RTX 3090 24GB（docker，另一次实测） | 上游 vllm-omni:v0.34.0 / 0.34.0 | 21.4 s | 86.8 s | 17.1–18.2 GiB/卡 |
| BF16 · 双卡 TP2 | 2 × RTX 3090 24GB（runveil，跨拓扑无 NVLink） | 上游 vllm-omni:v0.34.0 / 0.34.0 | 25.6 s | 124.9 s | 17.1–18.2 GiB/卡 |

说明：

- 只有 24–48 GB 消费卡（RTX 3090/4090）实测过；FP8 单卡两种架构（Ada 的 CUTLASS、Ampere 的 Marlin）都已在真实硬件上用本模板镜像 r1 验证，没有更大显存卡（A100/H100/H20）的数据。
- BF16 两档没有用镜像 r1 重测（本次租的 4090 只有 24 GB，没有 ≥ 40 GB 单卡或双卡机器），表里 BF16 的数字来自旧镜像或上游镜像，仅供参考；r1 与它们是同一个引擎版本线（vLLM-Omni 0.32 → 0.34）。
- 新旧镜像在同一型号的 FP8 单卡上差别不大：4090 上 r1 的 1024×1024 为 8.3 s，旧 0.32 镜像在 48GB 4090 上为 8.7–9.1 s，上游 0.34 镜像在另一台 24GB 租机上为 9.2–10.2 s（不同主机之间的差异包含主机本身，不能全算作镜像差别）。
- RTX 3090（SM86，Ampere）没有 FP8 硬件支持，FP8 比 4090（Ada，原生 W8A8）慢 3 倍以上（Marlin 的权重反量化开销）；需要更快的 FP8 可以直接用 BF16 + 更大显存的卡。
- 两卡 TP2 之间有无 NVLink 对延迟有影响：同一组 3090 上，NVLink 对（docker 示例）比跨 PCIe 拓扑（runveil 示例）快 1.18–1.44 倍，2048×2048 处差距更大。
- 冷启动（部署创建到健康检查通过）：权重已在共享存储上时，双卡 TP2 实测约 82 秒（含镜像拉取、但权重不需要下载）；首次下载权重另加权重大小 ÷ 下载带宽的时间。
- 编辑（`/v1/images/edits`，1024×1024 参考图）：本模板镜像 r1 上 4090 24GB 实测 9.4 s（预量化）/ 9.7 s（在线 FP8），3090 24GB 实测 29.9 s（预量化）；旧镜像双卡 TP2 BF16 为 26.7 s。
- 1344×768 与 768×1344 两个长宽比已在旧镜像（0.32）的双卡 TP2 BF16 部署上实测通过（各 24.6–24.8 s，与同尺寸的正方形档位接近），返回图片的像素尺寸与请求一致。

## 已知限制

- **没有 offload 档位**：vLLM-Omni 0.32/0.34 都不支持这个模型的逐层显存卸载，所以没有比 FP8 更省显存的路线；24 GB 以下的卡跑不动。
- **Ampere FP8 较慢**：RTX 3090 用 Marlin W8A16，2048×2048 单张图接近 2 分钟；显存紧张但不在意速度时可用，追求速度建议用支持 FP8 的 Ada/Hopper 卡或 BF16 + 更大显存。
- **Ampere 卡上的 FP8 需要较新的 server**：早于 2026-11-24 的显存估算矩阵会拒绝 Ampere 上的 FP8（`dtype_unsupported_detail`），需要升级到包含该修复的版本才能在 RTX 3090 一类的 Ampere 卡上使用这个档位；Ada 及以上不受影响。
- **首次请求较慢**：引擎启动后第一次推理要做图编译/预热，样本以外另有一次性开销，不计入上表中位数。

## 调用示例

文生图：

```bash
curl -X POST "$GPUWORKS_URL/v1/images/generations" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H "Content-Type: application/json" \
  --data '{"model":"<served-model>","prompt":"A sunrise over mountains","size":"1024x1024"}'
```

图像编辑（`image` 为源图文件）：

```bash
curl -X POST "$GPUWORKS_URL/v1/images/edits" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -F image="@input.png" \
  -F model=<served-model> \
  -F prompt="Make the background blue"
```

`size` 支持 1024x1024、1536x1536、2048x2048 三个正方形档位，以及 1344x768、768x1344 两个 16:9 附近的长宽比（均为 32 的倍数，已在真实部署上验证可用）。
