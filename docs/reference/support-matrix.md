# 厂商支持矩阵

| 厂商/框架 | 探测 | 整卡部署 | dGPU | 拓扑/备注 |
|---|---|---|---|---|
| NVIDIA / CUDA | nvidia-smi、sysfs | Docker、runveil、nerdctl | HAMi libvgpu.so（x86_64，glibc ≥ 2.17） | 支持 CUDA/NCCL 拓扑；驱动库由宿主机提供 |
| 昇腾 / CANN | npu-smi、驱动和 CANN 事实 | Docker + Ascend runtime；部分环境 runveil | vCANN；能力取决于宿主机 device-share 权限 | 外层/内层 CANN 版本不能混为一个结论 |
| PPU / HGGC | ppu-smi | provider/runtime 依赖 | HGGC 显存与加权 QoS | 20/80 长负载已证明繁忙时吞吐权重，不是硬 SM 分区 |
| 海光 / DTK | hy-smi | 由目录 spec 和 runtime 决定 | Z100 原生 vdev（实验性，x86_64 + Docker） | 需可选原生 helper、宿主库和逐卡行为探针；其他型号仍只支持整卡 |
| Apple / Metal | sysctl、ioreg（macOS 15+，Apple silicon） | host（引擎为宿主机进程，镜像是 darwin 环境包） | 无；一台 Mac 按统一内存同时跑多个实例 | 1 张统一内存卡，显存为 Metal 工作集；不支持引擎休眠、多机、KV 磁盘层；见 [ADR 0044](../adr/0044-mac-engines-as-host-processes.md) |

上表的 dGPU 能力状态由 server 的 partition capability（分区能力判定）推导：

- `ready`：dGPU 可用。
- `unavailable`：dGPU 不可用，附带 `reason` 说明原因，例如“缺少 dGPU 资产”“dGPU 探针失败”。

dGPU 不可用时，节点仍可使用整卡部署；平台不会把不满足 dGPU 条件的实例静默改成不限制显存的实例来启动。
昇腾实测组合、CANN 和 runtime 细节见[昇腾矩阵](ascend-support-matrix.md)；PPU 未完成项见[开发台账](../dev/ledgers/ppu-support-gaps.md)。


## PPU 设备身份与 dGPU 探针

PPU 注入使用 `ppu-smi` 返回并经设备节点核对的完整 UUID，按调度顺序写入 `CUDA_VISIBLE_DEVICES`。HGGC 将这些 UUID 映射为计算设备 `0..N-1`；Docker 的可见设备列表与 runveil 的宿主设备列表不必有相同编号。HGML 配额查询按同一 UUID 顺序定位配额与显存账本，不把 HGML 的宿主编号直接当作计算设备编号。

内嵌探针 `gpuworks/partition-probe:v4` 同时检查目标 UUID 和显存配额。缺失、无效、重复或不可查询的身份，以及只有其他卡显示配额的情况都不能通过。升级 agent 后需重新创建已有工作负载，新的设备选择与切分库才会生效；runveil 的设备可见性仍不等于内核设备隔离。

## 海光 Z100 dGPU

Z100 使用 `hygon-dgpu` 后端，按显存比例分配 CU。启用前，在节点构建并安装可选的 `gpuworks-hygon-vdev-helper`（构建步骤见源码的 `build/runtime-assets/hygon-vdev-helper/README.md`），并提供宿主 DTK、HYHAL 和 DCGM 库。agent 本身仍是静态二进制。仅 x86_64 + Docker 路径受支持；K100 等其他型号不会获得此能力。

逐卡探针必须确认 HIP 看到请求的显存配额、可分配 1 MiB，并拒绝超额 1 MiB 的分配。探针通过后才能调度 dGPU 实例。推理容器使用自己的 DTK；宿主 DTK 与 ABI 补充库只供轻量探针使用。内置 Qwen3-0.6B 的 `bf16-vllm-hygon-z100` spec 使用经过 Z100 验证的 SourceFind 镜像。

helper 在宿主锁下创建 vdev，并保存配置快照和归属令牌，阻止旧请求释放后来复用相同 ID 的 gpuworks vdev。容器移除成功后才释放 vdev；清理失败时保留记录，实例维持 `removing`，同一实例不会启动替代容器。agent 重启后不会自动扫描遗留记录，需人工核查；同机其他 vdev 管理工具若绕过 helper 的锁与令牌，不受此归属保护。

## GPU 型号与架构

server 收到节点上报时把型号名规范化后再存（`accelerator.NormalizeGPUName`）：去掉开头的厂商词
（`NVIDIA`、`Ascend`、`Huawei`、`Hygon`、`AMD`、`Apple`，后面须跟空白），首尾空白去掉、中间空白合并。
`NVIDIA GeForce RTX 4090` 与 `GeForce RTX 4090` 都存为 `GeForce RTX 4090`，`Ascend 910B4` 存为 `910B4`。
推理目录的名字谓词、PD 的同型号检查、GPU 列表读的都是这个值；原始名字只留在 agent 侧（`doctor`、日志）。
已有的 benchmark 快照保留当时的名字。

架构只存原始事实：NVIDIA 存 compute capability，其他厂商在 `arch_id` 存能确定的最具体身份。
代际（API 的 `arch_family`）与 NVIDIA 的 `sm_XY`（API 的 `arch_id`）由 `accelerator.GPUArch` 在读取时算出，
不入库；agent 上报的 `arch_family` 字段 server 不读。

| 厂商 | `arch_id`（存储） | 代际（推导，小写） |
|---|---|---|
| NVIDIA | 不存（由 compute capability 得 `sm_XY`） | `maxwell` 5.x、`pascal` 6.x、`volta` 7.0/7.2、`turing` 7.5、`ampere` 8.0–8.7、`ada` 8.9、`hopper` 9.x、`blackwell` 10.x–12.x |
| 昇腾 | 芯片型号：`910B4`、`910B2C`、`910C`、`310P3` | `910b`、`910c`、`310p`；初代 910 与认不出的为空 |
| 海光 | SKU：`K100AI`、`BW1000`；只报系列时存系列 `K100`；认不出为空 | `k100`、`z100`、`bw1000`、`bw1100` |
| PPU | `ZW810E`（取自名字） | `beethoven`（`ppu-smi` architecture 列） |
| Apple | SoC 名：`M1 Max`、`M4 Pro`（`ioreg` 的 model 去掉 `Apple`） | 芯片代际 `m1`…`m6` |

agent 探测不到任何加速卡（各厂商 CLI 都不应答、都没装、也没有厂商设备文件）时节点厂商为空，不再按 NVIDIA 上报。
