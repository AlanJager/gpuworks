# 历史验收：Qwen3.8-27B · AMD395 / gfx1151

**本文仅记录已退租 AMD395 主机上 r2/r5 的历史验收，不描述今天生成的包所使用的模型 revision 或权重大小。** 当前源码是启动配方 overlay：`prepare_import.py` 从明确选定的当前 seed spec 获取模型配置与 BF16 权重事实。其 revision 和大小可以不同于下文历史值，必须重新进行硬件验收。来源选择、显式镜像映射与精确镜像 ID 核验见[当前包使用说明](../../../../README.md)。

历史档位使用官方 `Qwen/Qwen3.8-27B` revision `1d4bf0f2ff6012fd82039f2fa52739d0dd7c60c0`。当时的 safetensors index 对应18 个 safetensors 分片，共 55,563,006,776 bytes，不含 tokenizer 和配置文件。权重当时单独获取，不包含在推理镜像内。这些数字标识旧验收输入，不是当前生成工具应输出的固定值。

vLLM r2 当时实测了单卡 BF16 配置：单序列、4096 token 上下文、eager 模式和 0.75 GPU 内存比例。SGLang r5 当时实测了 BF16、上下文 4096、最多 4 个运行请求，以及静态内存比例 0.75、Triton attention 与 linear-attention 后端、关闭 CUDA graph、总 token 上限 16384 和 Mamba 缓存 32 个 slot。SGLang 使用历史镜像 `sha256:c8de20ed7d138f6e3442c5995ad71a6810d097cb010032fbaec27106fef4cc1d` 在该主机通过了 8 条短文本请求，并发 1 和 4 各 4 条；当时保留了较早的 r2 SGLang 元数据供回滚。这些是该模型的 smoke 设置，不是通用引擎默认值，也不构成后续 seed revision 的验收。

历史配方要求单个 AMD GPU、架构 ID `gfx1151` 和 AMD395 系列名称。回退名称 `AMD GPU 0x1586 (gfx1151)` 只标识架构系列，不能确认具体 Ryzen 395 SKU。当时可用 UMA 取决于主机配置，128 GB 系统内存不代表 128 GB GPU 可分配内存。组件检查覆盖引擎导入、依赖一致性和 PyTorch 的 gfx1151 编译目标，本身没有证明 GPU 执行。

已记录的运行仅覆盖有限的 BF16 文本启动与聊天，没有证明图片输入、广泛回答质量、持续性能、长上下文、内存限制行为或多卡隔离。导入元数据不会传输镜像层，也不会验证新的 seed revision。重新验收请使用[当前 AMD 硬件验收指南](../../../../../../../docs/dev/amd-hardware-validation.md)。
