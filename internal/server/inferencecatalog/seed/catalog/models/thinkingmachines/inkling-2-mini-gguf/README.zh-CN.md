# TML Inkling 2 Mini（GGUF · llama.cpp）

在 Apple silicon Mac 上运行 Thinking Machines 的 Inkling 2 Mini，支持文本与图片输入。Inkling 2 Mini 是 26B 的混合专家模型，每个 token 激活约 3B 参数；24 层解码层里第 6–17 层循环执行三遍，共 48 层。引擎是上游 llama.cpp：b12600 起支持这个结构（`inkling2_loop`）及其图像编码器，环境包是 b12631 的 macOS 预编译版本。权重是 ggml-org 官方的 GGUF：IQ4_NL 模型（17.6 GB）与 Q8_0 视觉投影（346 MB）。

## 下载量

仓库把所有量化平铺放在一起（BF16、Q8_0、IQ4_NL 和两份视觉投影），模板按整个仓库下载，所以下载量是 98.8 GB，引擎实际加载其中 18.0 GB。磁盘请留出约 100 GB。

## 档位

| 档位 | 适用 | 准入门槛（Metal 可用内存） |
|---|---|---|
| `iq4nl-llamacpp-apple` | 36 GB 及以上内存的 Mac | 24 GiB |

实测（M4 Max Mac Studio，128 GB，模板默认的 64K 上下文）：常驻约 21 GB（18 GB 权重以内存映射方式载入，另有约 3.9 GB 进程内存：KV 缓存与计算缓冲）；约 5 秒加载完成；短提示解码约 35 tok/s，10K token 上下文时约 32 tok/s；10K token 提示的预填充约 300 tok/s。32 GB 的 Mac（默认工作集约 21 GiB）放不下。

## 参数

callable 档位没有参数字典，调整都在部署表单的「自定义参数」里追加 llama-server 的命令行参数，同名参数以后出现的为准。模板默认 `-c 65536`（上下文，四个并行槽位共用）、`-ngl 99`（全部层放 GPU）、`--jinja`（使用 GGUF 自带的对话模板）和 `--alias`（以部署的服务名应答）。大部分注意力层是 1024 token 的滑动窗口，加大上下文多占的内存不多。

## 思考内容

回复里模型的思考内容在 `reasoning_content`，与 `content` 分开。对话模板接受 `reasoning_effort`（`none`、`minimal`、`low`、`medium`、`high`（默认）、`xhigh`、`max`，或 0.0–0.99 的数），可以作为请求字段，也可以放在 `chat_template_kwargs` 里。它只是提示、不是开关：设为 `none` 时模型仍会写一段简短的思考。

## 已知限制

- 只接受静态图片：视觉投影不处理视频和音频。
- 引擎休眠、多机推理在 Mac 节点上不可用。
