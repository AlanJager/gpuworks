# 昆仑 P800 部署

## 当前能力

P800 PCIe / KL3 使用 `kunlunxin` 厂商和 `xre` 框架，已验证的运行时为 Linux amd64、Docker；需求列表或适配器条目不代表 nerdctl 已受支持。该组合支持整卡单卡、单机 TP（Tensor Parallelism，张量并行）。当前阶段不开放跨节点 TP。PP、引擎 DP、PD、分区和 sleep 尚未开放。多个独立单机副本不等于跨机 TP。

复用宿主已有 Docker daemon。独立 socket/data-root 不隔离宿主网络；不要在同一网络命名空间另起使用 `--bridge=none` 的 dockerd，这可能删除已有 `docker0`。镜像或推理测试不需要重启现有 Docker，离线探针使用容器级 `--network none`。

原生探测使用 `xpu-smi -q -x`。只兼容已观察到的 process_info 内 XML 闭合标签错误，其他 XML 错误仍失败；未知 ECC 状态显示遥测告警，不据此拒绝调度；已知不可纠正 ECC 错误使该卡不可调度，其余卡继续上报；PCI 清单补充 CLI 未列出的卡。只读库存可以使用命令缓存，设备注入必须重新核对 UUID、PCI BDF、minor、sysfs 字符设备；占用由公共计算进程清单持续上报。

GPUWorks 单机设备接入只核对并注入所选 XPU 和控制设备，不要求 XDR、RDMA 设备、IPC_LOCK 或 memlock 放宽。单机多卡的镜像与卡间通信组合仍须单独验证；这不等于承诺任意镜像都能运行。

## 镜像与模型档位

支持芯片并不自动提供任意模型的推理档位。当前仓库未发布默认昆仑 runner 镜像；使用现有镜像和推理模板导入 API 提供已经验证的镜像与用户 spec。

Qwen3-0.6B 和 Qwen3.8-27B 可使用仓库内的[可导入模板包](../../build/inference-templates/kunlun-p800/README.zh-CN.md)。它只添加用户档位；包内 spec 直接锁定已审核的私有仓库 manifest digest；可选 tag 镜像目录条目不是内置 runner，也不是公开发行镜像。

1. 在所选节点准备已验证镜像。记录不可变摘要、引擎版本及镜像中的实际 Python/入口路径，不以同名 CUDA 兼容字段判断原生运行能力。
2. 登记本地权重时填写完整仓库标识，例如 `deepseek-ai/DeepSeek-R1-Distill-Qwen-7B`。目录短名不是仓库标识；已有相同文件可以登记规范身份，无需复制或下载。
3. 导入 spec 时，`weights.sources[].repo_id` 必须与模型 `source_id` 对应，`gpus.vendor` 为 `kunlunxin`，卡名谓词匹配 `P800 PCIe`。缺少 spec 或身份不对应会导致 Web 无兼容档位。
4. spec 使用镜像中已验证的原生 entrypoint，明确 dtype、上下文和显存参数。依赖设备枚举的镜像可使用 `env -u XPU_VISIBLE_DEVICES`；不能把宿主编号当作容器逻辑编号。
5. 单卡使用 TP=1；单机多卡使用 TP=所选卡数、PP=1、nnodes=1。

## 验证与边界

先验证单卡，再验证所选单机卡组。Web 创建时选择正确档位、一个节点、TP=所选卡数及 PP=1；核对模型与镜像来源，再验证 health、普通 HTTP、SSE DONE 和网页文本。仅握手成功或收到 HTTP 200 不等于返回有效文本。跨节点请求当前返回不支持的放置理由。

相关支持边界见 [厂商支持矩阵](../reference/support-matrix.md)。

硬件目录显示 `Kunlunxin P800 PCIe`，厂商存为 `kunlunxin`；spec 匹配前 `NormalizeGPUName` 去掉品牌前缀，因此已审核的 `^P800 PCIe$` 谓词仍可匹配。硬件身份随运行时与模型组合的验证进入能力选择。Partition 可用性由动态分区能力检查给出，不设静态 Partition 功能轴。引擎 PD 模式仍由引擎 Profile 定义；厂商限制只定义在 accelerator 能力表，`ProfileForTarget` 根据它派生该目标可用的 PD 模式。
