# ADR 0019: 镜像目录只维护运行时事实，兼容性采用排序告警

日期: 2026-09-14 · 状态: 已接受

2026-09-19：新部署的默认命令模式及配置来源由 [ADR 0022](0022-template-owned-deployment-launch.md) 调整；存量部署保留原有语义。

## 背景

镜像目录过去同时保存厂商、GPU 型号标签、Variant、适配模型和 GPU 支持/排除列表。
这些自由文本与节点真实硬件经常不一致，无法覆盖新型号，也把模型知识错误地放到了镜像层。
驱动与 CUDA、CANN、DTK、HGGC 的公开兼容资料也持续变化，目录不可能穷举所有组合。

## 决策

- 镜像系列和版本只描述 framework、framework version、engine、engine version、主机架构及运行时默认值。厂商由 framework 的唯一映射推导；旧字段作为兼容列保留但不再读写或出现在 wire JSON。
- 自动选择先硬约束 engine、framework、厂商和主机架构。用户填写引擎版本时归一化后精确匹配；未填写时按数值版本降序选择系列中最新启用版本。同一组候选暂按稳定排序解决歧义。
- 驱动兼容性只影响候选排序并生成 Warning。CUDA 只采用最低驱动要求，不以 minor 版本表的上限淘汰更高驱动；无法证明的 CANN、DTK、HGGC 组合返回 Unknown。系统不产生 Perfect 判定。
- 用户显式指定的镜像始终可用，即使不在目录中或被判定可疑也不产生 Blocking。TrustedOverride 继续直接放行。
- `turing-no-bf16` 只对 NVIDIA 节点真实 compute capability 生效，条件为 `cc < 8.0`；它不参与镜像候选资格。

## 影响

节点探测和 `chipmatch` 是 GPU 型号、架构与能力的事实来源；推理模板负责模型家族和模型能力约束。
目录导入、API、UI 和 seed 不再生成或传播冗余标签。非 NVIDIA 节点在目录没有候选时不会误用 CUDA profile 默认镜像。
