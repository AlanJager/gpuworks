# DeepSeek-V4.1-Flash

配方来源：[SGLang 官方文档](https://docs.sglang.io/cookbook/autoregressive/DeepSeek/DeepSeek-V4_1)
与 [vLLM-Ascend DeepSeek-V4.1-Flash 教程](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/DeepSeek-V4.1-Flash.html)。

单节点：H200 × 8，或 B200/B300/GB300 × 4。原始混合 FP8/FP4 权重约
510.3 GB；需要额外显存、主机内存和下载空间。使用专用 dev-dsv41 镜像；不能替换成
V4 镜像或普通旧版 runner。Hopper 使用不同 MoE 内核，Blackwell 配方启用内置
DSpark。

昇腾 A3 单机使用 `Eco-Tech/DeepSeek-V4.1-Flash-w8a8`，权重约 797.6 GB。
模板在一台 64 GB × 16 逻辑 die 的 A3/910C 上运行 DP4 × TP4，Engram 表放主机内存；
默认上下文 150K。权重体积接近整机 HBM 容量，启动可能持续数十分钟，因此环境把
引擎就绪超时设为 36,000 秒。

上游已标记 NVIDIA 配方验证通过；本平台已实测 B200 × 4 配方（见下），H200 × 8 和
昇腾 A3 配方仍待真机完成部署与 chat 验收。

未声明的 AMD、海光、PPU、Apple 组合不在本模板覆盖范围内。

## 实测（B200 × 4，TP4，DSPARK，2026-10-19）

Vast 容器节点、runveil 运行时。对话、思考模式（`chat_template_kwargs.thinking=true`）、工具调用通过；单路 964 token 输出约 258 token/s，TTFT 0.49 s（经网关），DSPARK 接受长度 2.0–3.7；启动到就绪约 21 分钟（JIT 编译内核）。未做多并发压测，H200 × 8 未实测。
