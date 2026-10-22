# Qwen3.8-Flash-Next 125B（GGUF · llama.cpp）

在 Apple silicon Mac 上运行 125B 的 Qwen3.8-Flash-Next。引擎是上游 llama.cpp：b10660 起支持这个模型的结构（qwen4exp，包括它的 n-gram 嵌入表），环境包是 b11617 的 macOS 预编译版本。权重与 NVIDIA 上的 Strata 模板相同：ISTA-DASLab 的 GSQ-RCO IQ2_XS 量化，两个分片共约 68 GB。

## 两个档位

| 档位 | 适用 | 准入门槛（Metal 可用内存） |
|---|---|---|
| `iq2xs-llamacpp-apple-128g` | 128 GB 内存的 Mac（默认工作集约 96 GiB） | 80 GiB |
| `iq2xs-llamacpp-apple-64g` | 64 GB 内存的 Mac（默认工作集约 48 GiB） | 44 GiB |

两个档位用同一份权重。llama.cpp 默认把超过 4 GiB 的张量留在 SSD 上按需读取（`--lazy-mode auto`），这份 GGUF 把 n-gram 表单独放在第二个分片（28.8 GB），所以常驻 GPU 的主要是第一个分片（39.2 GB）和 KV 缓存。KV 很小：48 层里只有 12 层是完整注意力，约 24 KiB/token，128K 上下文约 3 GiB。

实测（M4 Max Mac Studio，128 GB）：GPU 实际占用约 39 GiB；解码 35–45 tok/s；预填充约 550 tok/s（8K token 提示）。把同一台机器的 `iogpu.wired_limit_mb` 临时设为 49152 模拟 64 GB Mac：GPU 占用 39.3 GiB，速度不变。真实的 64 GB Mac 物理内存更少，n-gram 表会更多地从 SSD 读取（每 token 只读几 KB），速度尚未在真机上测过。磁盘至少留出 70 GB。

## 参数

callable 档位没有参数字典，调整都在部署表单的「自定义参数」里追加 llama-server 的命令行参数，同名参数以后出现的为准。模板默认 `-c 32768`（上下文）、`-ngl 99`（全部层放 GPU）、`--jinja`（使用 GGUF 自带的对话模板，含工具调用与思考内容）。上下文越大，KV 缓存占的内存越多。

## 已知限制

- 上游实现里 n-gram 嵌入的卷积不跨 ubatch 携带状态，只对从位置 0 开始的预填充完全正确；分块预填充的状态处理上游标注为未完成。
- 只接受文本输入：视觉投影文件不在下载范围内。
- 引擎休眠、多机推理在 Mac 节点上不可用。
