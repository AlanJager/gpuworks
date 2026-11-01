# PPU 支持缺口与证据台账

本文件记录 `model-inference-validation/vendors/ppu` 合并账本中已经 `REAL_PASS`、但 gpuworks 尚未具备可部署契约的条目。
研究通过不等于 gpuworks 已具备可部署契约；无法从公开/仓库证据还原镜像、entrypoint 或原生运行时的条目列在“缺口”状态。

这里的“可部署契约”指 GPUWorks 能据以部署模型的明确约定，包括使用哪个镜像、怎样启动、需要哪些运行时资产、
提供什么 API，以及适用的拓扑。entrypoint 是启动入口，catalog 是推理目录；spec 是可执行档位，
profile 保留历史记录的称呼。当前术语定义见 [`CONTEXT.md`](../../../CONTEXT.md)。

下面的表格是 2026-09-08 前后的一次快照（当时账本为 65 个 `REAL_PASS` 唯一模型），保留用于对照历史结论。
当前口径以 `pipeline/ledger/current/ppu-full-real-validation.json` 为准（2026-09-14 生成：`real_pass_unique_models: 108`、`large_tp_or_pp_real_pass_unique_models: 5`），
不再用本文件的快照行数代表覆盖率。

## 镜像来源口径

同一个模型在不同 run 里可能跑在不同的 PPU 运行时上，而它们的镜像不是同一件事：

| 研究引擎标签 | 实际镜像 | 说明 |
|---|---|---|
| `asllm-2.0.0-vllm-0.23.0` | `https://containerimages.zstack.ai/asllm_2.0.0-pytorch2.10.0-ubuntu24.04-sail2.1.0-cuda13.0-sglang0.5.13-vllm0.23.0-py312.tar` | 厂商 ASLLM 镜像，自带 vLLM 0.23.0 / SGLang 0.5.13；打包为 tar，无 registry digest |
| `asllm-1.10.4-vllm-0.23.0` | `https://containerimages.zstack.ai/asllm_1.10.4-pytorch2.10.0-ubuntu24.04-sail2.1.0-cuda13.0-sglang0.5.12-vllm0.23.0-py312.tar` | 上一版 ASLLM；自带 SGLang 0.5.12 |
| `legacy-vllm-0.23.0` / `vllm-0.23.0` | `swr.cn-south-1.myhuaweicloud.com/gpustack/runner:hggc13.0-vllm0.23.0` | GPUStack runner，研究 worker 的 vLLM 回退路径 |
| `sglang-0.5.12` | `swr.cn-south-1.myhuaweicloud.com/gpustack/runner:hggc13.0-sglang0.5.12` | GPUStack runner 的 SGLang 路径 |

catalog 里每个 PPU spec 的 `image` 必须是它自己那次 PASS 用过的镜像，不能按模型推断；`int4-vllm-ppu-tp2-a6977d07`（Laguna-S-2.1）就是例子：TP2 在 `asllm-1.10.4` 上通过，在 `asllm-2.0.0` 上只有 `COMPAT_FAIL`。

`internal/server/inferencecatalog/seed/ppu_profiles_test.go` 把每个 profile 的 engine / image / tested revision / weight_bytes 一起钉住，改动其中任何一项都会失败。


Hugging Face 与 ModelScope 的可用性采用 `pipeline/ledger/current/ppu-full-real-combined-progress.json` 中对应 tested revision 的 `source_availability` 记录；模板只写入已核实可用的 provider。当前工作区对外部 API 的即时 HTTP 请求不可用时，不以网络超时推断仓库不存在。

下表的 tested revision 是该次测试使用的模型版本。`template-generated` 表示已生成模板，`gap` 表示仍缺部署信息；
来源列中的 `available`、`unavailable (404)`、`not recorded` 分别表示记录为可用、返回 404、未记录。
这些列分别描述模板准备状态和模型来源可用性，均不能单独代替 GPUWorks 部署验收结果。

| 模型 | 状态 | 研究引擎 | tested revision | Hugging Face | ModelScope | 备注 |
|---|---|---|---|---|---|---|
| `ByteDance-Seed/Seed-OSS-36B-Instruct` | template-generated | `vllm-0.23.0` | `497f1dca95ebdec98e41d517b9f060ee753c902f` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `FunAudioLLM/Fun-ASR-Nano-2512-hf` | gap | `transformers-native` | `972ee603a3abb40a66c802ffaa9ba0ce88742b60` | available | unavailable (404) | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |
| `HuggingFaceTB/SmolLM-135M-Instruct` | template-generated | `vllm-0.23.0` | `fcc320f490e08fdb4b99d935b2c58d40bf35b0d0` | available | available | PPU vLLM/SGLang digest template |
| `HuggingFaceTB/SmolLM2-1.7B-Instruct` | template-generated | `vllm-0.23.0` | `31b70e2e869a7173562077fd711b654946d38674` | available | available | PPU vLLM/SGLang digest template |
| `HuggingFaceTB/SmolLM2-135M-Instruct` | template-generated | `vllm-0.23.0` | `12fd25f77366fa6b3b4b768ec3050bf629380bac` | available | available | PPU vLLM/SGLang digest template |
| `HuggingFaceTB/SmolLM2-360M-Instruct` | template-generated | `vllm-0.23.0` | `a10cc1512eabd3dde888204e902eca88bddb4951` | available | available | PPU vLLM/SGLang digest template |
| `LiquidAI/LFM2.5-1.2B-Instruct` | template-generated | `vllm-0.23.0` | `0f604ada3f766f9f257460c4c9f0b5d6f69d431b` | available | available | PPU vLLM/SGLang digest template |
| `LiquidAI/LFM2.5-230M` | template-generated | `vllm-0.23.0` | `40cb2ad3b3044d5a41eee083a6103c8b523afa45` | available | available | PPU vLLM/SGLang digest template |
| `LiquidAI/LFM2.5-350M` | template-generated | `vllm-0.23.0` | `9e6c6ccf47cd318696e137d381a7ded8fe4df09f` | available | available | PPU vLLM/SGLang digest template |
| `LiquidAI/LFM2.5-VL-450M` | template-generated | `vllm-0.23.0` | `fc6221ca597f3315e4f82fc2df606783267b34ba` | available | available | PPU vLLM/SGLang digest template |
| `OpenOneRec/OneRec-1.7B` | template-generated | `vllm-0.23.0` | `cc4d0b5b7294ecf75e40be1c77fa6b7d284bb84b` | available | available | PPU vLLM/SGLang digest template |
| `PaddlePaddle/PaddleOCR-VL-1.6` | template-generated | `vllm-0.23.0` | `c5630abae1d940eafe0697512a0325494b02ab42` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen1.5-0.5B-Chat` | template-generated | `vllm-0.23.0` | `4d14e384a4b037942bb3f3016665157c8bcb70ea` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2-0.5B` | template-generated | `vllm-0.23.0` | `91d2aff3f957f99e4c74c962f2f408dcc88a18d8` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2-0.5B-Instruct` | template-generated | `vllm-0.23.0` | `c540970f9e29518b1d8f06ab8b24cba66ad77b6d` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2-1.5B-Instruct` | template-generated | `vllm-0.23.0` | `ba1cf1846d7df0a0591d6c00649f57e798519da8` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2.5-0.5B-Instruct` | template-generated | `vllm-0.23.0` | `7ae557604adf67be50417f59c2c2f167def9a775` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2.5-1.5B` | template-generated | `vllm-0.23.0` | `8faed761d45a263340a0528343f099c05c9a4323` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2.5-1.5B-Instruct` | template-generated | `vllm-0.23.0` | `989aa7980e4cf806f80c7fef2b1adb7bc71aa306` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen2.5-Coder-0.5B-Instruct` | template-generated | `vllm-0.23.0` | `ea3f2471cf1b1f0db85067f1ef93848e38e88c25` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-0.6B` | template-generated | `vllm-0.23.0` | `c1899de289a04d12100db370d81485cdf75e47ca` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-1.7B` | template-generated | `vllm-0.23.0` | `70d244cc86ccca08cf5af4e1e306ecf908b1ad5e` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-1.7B-Base` | template-generated | `vllm-0.23.0` | `ea980cb0a6c2ae4b936e82123acc929f1cec04c1` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-4B` | template-generated | `vllm-0.23.0` | `1cfa9a7208912126459214e8b04321603b3df60c` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-4B-Instruct-2507` | template-generated | `vllm-0.23.0` | `cdbee75f17c01a7cc42f958dc650907174af0554` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-8B` | template-generated | `vllm-0.23.0` | `b968826d9c46dd6066d109eabc6255188de91218` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3-ASR-0.6B-hf` | gap | `transformers-native` | `7f1569a48a89f3e3f4dc3a5c9d28bddd903bc76c` | available | available | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |
| `Qwen/Qwen3-TTS-12Hz-0.6B-Base` | gap | `qwen-tts-native` | `5d83992436eae1d760afd27aff78a71d676296fc` | available | available | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |
| `Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice` | gap | `qwen-tts-native` | `85e237c12c027371202489a0ec509ded67b5e4b5` | available | available | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |
| `Qwen/Qwen3-VL-Embedding-2B` | template-generated | `vllm` | `9f2f7e710d6d81056aa5c0a4f04764fec6bb7bda` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-0.8B` | template-generated | `vllm-0.23.0` | `2fc06364715b967f1860aea9cf38778875588b17` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-122B-A10B-GPTQ-Int4` | template-generated | `vllm-0.23.0` | `30cd92cba9707a9aba09d1e490ed4b66b78e9606` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-2B` | template-generated | `vllm-0.23.0` | `15852e8c16360a2fea060d615a32b45270f8a8fc` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-4B` | template-generated | `vllm-0.23.0` | `851bf6e806efd8d0a36b00ddf55e13ccb7b8cd0a` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.5-9B` | template-generated | `vllm-0.23.0` | `c202236235762e1c871ad0ccb60c8ee5ba337b9a` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3.8-27B` | template-generated | `vllm` | `1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3Guard-Gen-0.6B` | template-generated | `vllm-0.23.0` | `fada3b2f655b89601929198343c94cd2f64d93cc` | available | available | PPU vLLM/SGLang digest template |
| `Qwen/Qwen3Guard-Gen-4B` | template-generated | `vllm-0.23.0` | `6ec42827da0c1ff11e7a49dc269d2e810d27e108` | available | available | PPU vLLM/SGLang digest template |
| `TinyLlama/TinyLlama-1.1B-Chat-v1.0` | template-generated | `vllm-0.23.0` | `fe8a4ea1ffedaf415f4da2f062534de366a451e6` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `WeiboAI/VibeThinker-3B` | template-generated | `vllm-0.23.0` | `77bd2cced09193c8b9a59a32bd8577bbd1f3e01c` | available | available | PPU vLLM/SGLang digest template |
| `bosonai/higgs-audio-v3-tts-4b` | gap | `asllm-2.0.0-vllm-0.23.0` | `7556c17e05201fccd9c8cc120bc216dcc7b5d561` | not recorded | not recorded | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产；参考音频尚缺编码器组件与离线验证 |
| `deepreinforce-ai/Ornith-1.0-9B` | template-generated | `vllm-0.23.0` | `83dc1f5e24ef8527af019a6b3bf66ac0f1c2c999` | available | available | metadata incomplete: current_weight_payload_first_published_at |
| `deepseek-ai/DeepSeek-OCR` | template-generated | `vllm-0.23.0` | `9f30c71f441d010e5429c532364a86705536c53a` | available | available | PPU vLLM/SGLang digest template |
| `deepseek-ai/DeepSeek-OCR-2` | template-generated | `vllm` | `aaa02f3811945a91062062994c5c4a3f4c0af2b0` | available | available | PPU vLLM/SGLang digest template |
| `deepseek-ai/DeepSeek-V4-Flash` | gap | `vllm-0.23.0` | `60d8d70770c6776ff598c94bb586a859a38244f1` | available | available | 研究记录绑定 PP4，多阶段拓扑当前无法由模板 placement 契约完整表达 |
| `google/embeddinggemma-300m` | template-generated | `vllm-0.23.0` | `57c266a740f537b4dc058e1b0cda161fd15afa75` | available | available | PPU vLLM/SGLang digest template |
| `google/gemma-4-E2B-it-qat-w4a16-ct` | template-generated | `vllm-0.23.0` | `971342c08f607aa7779983f6b5289778b5d271a7` | available | available | PPU vLLM/SGLang digest template |
| `ibm-granite/granite-4.1-3b` | template-generated | `vllm-0.23.0` | `c0650403e44e78ec0262dab1c90914c65b196c4e` | available | available | PPU vLLM/SGLang digest template |
| `ibm-granite/granite-4.2-3b` | template-generated | `vllm` | `b7e947307dd2efb3ad3b853b0e8a7e75f8ad4ac2` | available | available | PPU vLLM/SGLang digest template |
| `inclusionAI/LLaDA2.1-mini` | template-generated | `sglang-0.5.12` | `20e64e2ad21644d0e5248586ed9c942cdd45de0f` | available | available | PPU vLLM/SGLang digest template |
| `jinaai/jina-embeddings-v5-text-small` | template-generated | `vllm` | `dd76d535f5447ca3897a9c893fb1e612ead98192` | available | available | PPU vLLM/SGLang digest template |
| `jinaai/jina-embeddings-v5-text-small-clustering` | template-generated | `vllm-0.23.0` | `4ab9d9d3c5cca288d9ab38c1dc5228124fdf3271` | available | available | PPU vLLM/SGLang digest template |
| `meituan-longcat/LongCat-Image-Edit` | gap | `asllm-2.0.0-vllm-omni` | `7b54ef423aa7854be7861600024be5c56ab7875a` | available | available | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |
| `microsoft/Phi-4-mini-instruct` | template-generated | `vllm-0.23.0` | `cfbefacb99257ffa30c83adab238a50856ac3083` | available | available | PPU vLLM/SGLang digest template |
| `mistralai/Shieldstral-1.0-3B` | template-generated | `vllm` | `003ec7e2b0bab5f0e6307edbaf186fa5822b76f5` | available | available | PPU vLLM/SGLang digest template |
| `mistralai/Voxtral-4B-TTS-2603` | gap | `asllm-2.0.0-vllm-omni` | `b81be46c3777f88621676791b512bb01dc1cb970` | available | available | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |
| `nvidia/Cosmos3-Nano` | gap | `asllm-2.0.0-vllm-omni` | `7a312c868bcce8e40b3eb40861300a9d0ba3fde1` | available | unavailable (404) | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |
| `nvidia/NVIDIA-Nemotron-3-Nano-4B-BF16` | template-generated | `vllm-0.23.0` | `dfaf35de3e30f1867dd8dbc38a7fc9fb52d3914f` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `nvidia/Nemotron-3-Embed-1B-BF16` | template-generated | `vllm` | `c0c9fea93ea424587517f2c59e20db9f1d6bf615` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `nvidia/nemotron-3.5-asr-streaming-0.6b` | gap | `transformers-native` | `1c8deaecc64b91f034d73e08dd8b64625eb3395d` | available | unavailable (404) | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |
| `openai/gpt-oss-20b` | template-generated | `vllm-0.23.0` | `6cee5e81ee83917806bbde320786a8fb61efebee` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `openbmb/MiniCPM5-1B` | template-generated | `vllm-0.23.0` | `87179e5c1f455ef22e6223592d2d61351b525bfc` | available | available | PPU vLLM/SGLang digest template |
| `zai-org/GLM-ASR-Nano-2512` | gap | `transformers-native` | `61ba4e0b3309b6656edea3e93e419f7bd5c61957` | available | unavailable (404) | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |
| `zai-org/GLM-OCR` | template-generated | `vllm` | `ca5d8b3e287e52589e37c28385d9655ee4372f9d` | available | unavailable (404) | PPU vLLM/SGLang digest template |
| `zai-org/GLM-TTS` | gap | `asllm-2.0.0-vllm-omni` | `b3feb691fa4cf590eec4418f6275f95220e58844` | not recorded | not recorded | 原生/Omni 路径缺少现有引擎可执行契约；待 callable runtime/entrypoint/资产 |

## catalog 中每个 PPU profile 的通过证据

下表是本轮从 PPU 运行记录收编进 `internal/server/inferencecatalog/seed/catalog/` 的 31 个 profile 及其对应的真实通过 run（目录里还有更早就存在的其它 PPU profile，不在本表内）。
run 目录在研究仓库的 PPU 节点上（`/bmcp_lvm_fs/gpuworks_weiw/ppu-model-research/runs/<run-id>`），本仓库不复刻原始 evidence 包。
engine 列是研究账本里的引擎标签，与上表的镜像一一对应。

| catalog model | profile | 研究引擎 | PASS run id |
|---|---|---|---|
| `baidu/ernie-4.5-21b-a3b-pt` | `bf16-vllm-ppu-87db9548` | `asllm-2.0.0-vllm-0.23.0` | `full-baidu__ernie-4.5-21b-a3b-pt-asllm-2.0.0-vllm-0.23.0-full-real-20260904T211715` |
| `deepseek-ai/deepseek-v4-flash` | `fp8-vllm-ppu-pp4-60d8d707` | `vllm-0.23.0` | `full-big-deepseek-ai__deepseek-v4-flash-tp4-pp4-fallback-vllm023-full-real-20260901T145833` |
| `google/gemma-4-e4b-it-qat-w4a16-ct` | `int4-vllm-ppu-6cd26aaa` | `asllm-2.0.0-vllm-0.23.0` | `catalog-google__gemma-4-e4b-it-qat-w4a16-ct-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260913T173027` |
| `google/translategemma-4b-it` | `bf16-vllm-ppu-10042cb0` | `asllm-2.0.0-vllm-0.23.0` | `full-google__translategemma-4b-it-asllm-2.0.0-vllm-0.23.0-full-real-20260905T231630` |
| `hugging-quants/meta-llama-3.1-8b-instruct-awq-int4` | `awq-vllm-ppu-db1f81ad` | `asllm-2.0.0-vllm-0.23.0` | `full-hugging-quants__meta-llama-3.1-8b-instruct-awq-int4-asllm-2.0.0-vllm-0.23.0-full-real-20260909T052641` |
| `internlm/intern-s1-mini` | `bf16-vllm-ppu-0f25c8f6` | `asllm-2.0.0-vllm-0.23.0` | `full-internlm__intern-s1-mini-asllm-2.0.0-vllm-0.23.0-full-real-20260909T052642` |
| `liquidai/lfm2.5-1.2b-base` | `bf16-vllm-ppu-7453bca9` | `asllm-2.0.0-vllm-0.23.0` | `full-liquidai__lfm2.5-1.2b-base-asllm-2.0.0-vllm-0.23.0-full-real-20260906T121415` |
| `liquidai/lfm2.5-1.2b-jp` | `bf16-vllm-ppu-58ef35ca` | `asllm-2.0.0-vllm-0.23.0` | `full-liquidai__lfm2.5-1.2b-jp-asllm-2.0.0-vllm-0.23.0-full-real-20260906T061422` |
| `liquidai/lfm2.5-1.2b-jp-202606` | `bf16-vllm-ppu-52b8b447` | `asllm-2.0.0-vllm-0.23.0` | `full-liquidai__lfm2.5-1.2b-jp-202606-asllm-2.0.0-vllm-0.23.0-full-real-20260906T150310` |
| `liquidai/lfm2.5-8b-a1b` | `bf16-vllm-ppu-5dd22602` | `asllm-2.0.0-vllm-0.23.0` | `full-liquidai__lfm2.5-8b-a1b-asllm-2.0.0-vllm-0.23.0-full-real-20260907T121750` |
| `microsoft/phi-4-multimodal-instruct` | `bf16-vllm-ppu-93f923e1` | `asllm-2.0.0-vllm-0.23.0` | `catalog-microsoft__phi-4-multimodal-instruct-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260906T060523` |
| `mistralai/ministral-3-14b-reasoning-2512` | `bf16-vllm-ppu-51f9210f` | `legacy-vllm-0.23.0` | `catalog-mistralai__ministral-3-14b-reasoning-2512-vision-legacy-vllm-0.23.0-full-real-20260911T000357` |
| `mistralai/ministral-3-3b-reasoning-2512` | `bf16-vllm-ppu-4a36357c` | `asllm-2.0.0-vllm-0.23.0` | `catalog-mistralai__ministral-3-3b-reasoning-2512-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260906T013012` |
| `mistralai/ministral-3-8b-reasoning-2512` | `bf16-vllm-ppu-81eaece1` | `asllm-2.0.0-vllm-0.23.0` | `catalog-mistralai__ministral-3-8b-reasoning-2512-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260910T012942` |
| `nvidia/nvidia-nemotron-3.5-lightning-30b-a3b-bf16` | `bf16-vllm-ppu-a9904d24` | `asllm-2.0.0-vllm-0.23.0` | `full-nvidia__nvidia-nemotron-3.5-lightning-30b-a3b-bf16-asllm-2.0.0-vllm-0.23.0-full-real-20260912T105437` |
| `opengvlab/internvl3_5-8b` | `bf16-vllm-ppu-9bb6a56a` | `asllm-2.0.0-vllm-0.23.0` | `catalog-opengvlab__internvl3_5-8b-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260904T000817` |
| `poolside/laguna-s-2.1` | `int4-vllm-ppu-tp2-a6977d07` | `asllm-1.10.4-vllm-0.23.0` | `full-big-poolside__laguna-s-2.1-int4-tp2-asllm-1.10.4-vllm-0.23.0-full-real-20260911T215920` |
| `qwen/qwen2.5-coder-1.5b-instruct` | `bf16-vllm-ppu-2e1fd397` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen2_5_coder_1_5b_instruct-asllm-2.0.0-vllm-0.23.0-full-real-20260908T184259` |
| `qwen/qwen2.5-vl-72b-instruct` | `int4-vllm-ppu-vision-c8b87d4b` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen__qwen2.5-vl-72b-instruct-awq-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260912T203447` |
| `qwen/qwen2.5-vl-7b-instruct` | `int4-vllm-ppu-vision-536a3579` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen__qwen2.5-vl-7b-instruct-awq-fp16-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260906T221946` |
| `qwen/qwen3-14b` | `fp8-sglang-ppu-no-piecewise-9a283b4a` | `sglang-0.5.12` | `full-qwen__qwen3-14b-fp8-graph-no-piecewise-sglang0512-full-real-20260907T212023` |
| `qwen/qwen3-235b-a22b-instruct-2507` | `bf16-vllm-ppu-tp8-ac9c66cc` | `asllm-1.10.4-vllm-0.23.0` | `full-big-qwen__qwen3-235b-a22b-instruct-2507-tp8-asllm-1.10.4-vllm-0.23.0-full-real-20260911T222202` |
| `qwen/qwen3-30b-a3b` | `bf16-vllm-ppu-ad44e777` | `legacy-vllm-0.23.0` | `full-qwen__qwen3-30b-a3b-legacy-vllm-0.23.0-full-real-20260911T044537` |
| `qwen/qwen3-32b-awq` | `awq-vllm-ppu-0499c3ac` | `asllm-2.0.0-vllm-0.23.0` | `full-qwen__qwen3-32b-awq-text-fp16-asllm-2.0.0-vllm-0.23.0-full-real-20260906T224848` |
| `qwen/qwen3-4b` | `fp8-sglang-ppu-no-piecewise-96b30dc1` | `sglang-0.5.12` | `full-qwen__qwen3-4b-fp8-graph-no-piecewise-sglang0512-full-real-20260907T224109` |
| `qwen/qwen3-vl-32b-instruct` | `fp8-sglang-ppu-4bf2c2f3` | `sglang-0.5.12` | `full-qwen__qwen3-vl-32b-instruct-fp8-fallback-sglang0512-full-real-20260910T053304` |
| `qwen/qwen3.5-27b-gptq-int4` | `int4-vllm-ppu-8f0c09f2` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen__qwen3.5-27b-gptq-int4-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260913T174629` |
| `qwen/qwen3.5-35b-a3b-gptq-int4` | `int4-vllm-ppu-3af5ca29` | `asllm-2.0.0-vllm-0.23.0` | `catalog-qwen__qwen3.5-35b-a3b-gptq-int4-vision-asllm-2.0.0-vllm-0.23.0-full-real-20260913T132754` |
| `qwen/qwen3.5-397b-a17b` | `int4-vllm-ppu-tp4-df333de3` | `asllm-1.10.4-vllm-0.23.0` | `full-big-qwen__qwen3.5-397b-a17b-gptq-int4-tp4-asllm-1.10.4-vllm-0.23.0-full-real-20260911T203042` |
| `qwen/qwen3guard-gen-8b` | `bf16-vllm-ppu-4505cb1a` | `asllm-2.0.0-vllm-0.23.0` | `full-qwen__qwen3guard-gen-8b-asllm-2.0.0-vllm-0.23.0-full-real-20260907T170425` |
| `zai-org/glm-4.7-flash` | `bf16-vllm-ppu-7dd20894` | `legacy-vllm-0.23.0` | `full-zai-org__glm-4.7-flash-legacy-vllm-0.23.0-full-real-20260911T122405` |

## 卡数约束只表达拓扑，不表达证据等级

2026-09-15：删除了 79 个 PPU spec 上"只接受 1 卡"的 resolver 约束。

原先每个 PPU spec 的 resolver 都带 `if gpu.count != N: reject(...)`，而 N 就是那次通过 run 用的卡数。
对绝大多数模型（79 个）N=1，于是这个约束实际表达的是"我们只测过 1 张卡"——
一个**证据声明**被写成了**运行时门禁**，代价是这些 profile 在 2/4/8 卡的机器上直接不可用，
而它们的权重最大也只有 72 GB，单卡 96 GiB 装得下、多卡也同样跑得动标准 TP 布局。

现在：

- **只有拓扑要求才钉卡数**：剩下 5 个（DeepSeek-V4-Flash PP4、Laguna-S-2.1 TP2、Qwen3-235B TP8、Qwen3.5-397B TP4、Qwen3.5-122B-A10B TP2），名字里都带 `-tp`/`-pp`。
- **其余 79 个不钉**，卡数由调度器决定，TP = 分到的卡数——与目录里 NVIDIA spec 的行为一致（NVIDIA spec 从来没有 placement 块）。
- **实测卡数留在证据里**：研究账本 `pipeline/ledger/current/ppu-full-real-validation.json` 每条记录的
  `parallelism.tensor_parallel_size` / `pipeline_parallel_size` 就是那次通过 run 的实际拓扑。抽查确认上述 79 个都是 `tp=1 pp=1`。
- `TestPPUCardCountPinsExpressTopologyNotEvidence` 守住这条线：PPU spec 若钉卡数，必须 >1 且名字里带拓扑标记，
  防止"证据声明"再次伪装成门禁。

放开后的代价要说清楚：用户手选 3/5/6 张卡时也会被接受（TP=3 之类），
若该模型的注意力头数不被 3 整除，会在引擎启动时才失败。
这与 NVIDIA spec 的现状一致；要收紧的话，加一条卡数门禁即可，但那是把**调度器的 2 的幂启发式**写成模型属性，本仓库刻意没有这么做。


## 仍需补齐的部署信息

- `Qwen/Qwen3-VL-32B-Instruct-FP8`（PPU profile `fp8-sglang-ppu-4bf2c2f3`）：PASS 只跑过文本探针（sentinel/算术/事实四问），研究 catalog 把它归 `kinds: ["text"]`，通用 worker 因此没有发过图片请求。

  profile 只声明模型本身的 `input_modalities: [text, image]`，名称不带 `vision` 标记；需要在 PPU 真机上补一次真实图片请求（`image_url.url` 的 base64 data URL 形式，与 Qwen2.5-VL 两个 profile 的验证方式相同）后才能升级为视觉合同。
- `Qwen/Qwen2.5-VL-7B-Instruct-AWQ` / `Qwen/Qwen2.5-VL-72B-Instruct-AWQ`：通过时的图片请求是 `image_url.url` 的 base64 data URL，不是 multipart，也没有独立图片字段；补样张时按同一形状构造。
- `FunAudioLLM/Fun-ASR-Nano-2512-hf`：研究账本为 `transformers-native`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
- `Qwen/Qwen3-ASR-0.6B-hf`：研究账本为 `transformers-native`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
- `Qwen/Qwen3-TTS-12Hz-0.6B-Base`：研究账本为 `qwen-tts-native`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
- `Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice`：研究账本为 `qwen-tts-native`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
- `bosonai/higgs-audio-v3-tts-4b`：研究账本为 `asllm-2.0.0-vllm-0.23.0`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。 PPU 档位虽声明 `ref_audio`，但未声明 `audio-tokenizer` 辅助组件，也未验证本地编码器加载和真实参考音频请求；NVIDIA 档位的修复不代表 PPU 已通过。
- `meituan-longcat/LongCat-Image-Edit`：研究账本为 `asllm-2.0.0-vllm-omni`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
- `mistralai/Voxtral-4B-TTS-2603`：研究账本为 `asllm-2.0.0-vllm-omni`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
- `nvidia/Cosmos3-Nano`：研究账本为 `asllm-2.0.0-vllm-omni`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
- `nvidia/nemotron-3.5-asr-streaming-0.6b`：研究账本为 `transformers-native`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
- `zai-org/GLM-ASR-Nano-2512`：研究账本为 `transformers-native`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
- `zai-org/GLM-TTS`：研究账本为 `asllm-2.0.0-vllm-omni`；需要从原始运行记录补齐 callable/native entrypoint、运行时资产、API contract 和镜像 digest 后才能转成正式 gpuworks spec。
