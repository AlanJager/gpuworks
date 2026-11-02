# 厂商支持矩阵

| 厂商/框架 | 探测 | 整卡部署 | dGPU | 拓扑/备注 |
|---|---|---|---|---|
| NVIDIA / CUDA | nvidia-smi、sysfs | Docker、runveil、nerdctl | HAMi libvgpu.so（x86_64，glibc ≥ 2.17） | 支持 CUDA/NCCL 拓扑；驱动库由宿主机提供 |
| 昇腾 / CANN | npu-smi、驱动和 CANN 事实 | Docker + Ascend runtime；部分环境 runveil | vCANN；能力取决于宿主机 device-share 权限 | 外层/内层 CANN 版本不能混为一个结论 |
| PPU / HGGC | ppu-smi | provider/runtime 依赖 | HGGC 显存与加权 QoS | 20/80 长负载已证明繁忙时吞吐权重，不是硬 SM 分区 |
| 海光 / DTK | hy-smi | 由目录 spec 和 runtime 决定 | Z100 / K100_AI 原生 vdev（实验性，x86_64 + Docker） | 需可选原生 helper、宿主库和逐卡行为探针；其他型号仍只支持整卡 |
| Apple / Metal | sysctl、ioreg（macOS 15+，Apple silicon） | host（引擎为宿主机进程，镜像是 darwin 环境包） | 无；一台 Mac 按统一内存同时跑多个实例 | 1 张统一内存卡，显存为 Metal 工作集；不支持引擎休眠、多机、KV 磁盘层；见 [ADR 0044](../adr/0044-mac-engines-as-host-processes.md) |

上表的 dGPU 能力状态由 server 的 partition capability（分区能力判定）推导：

- `ready`：dGPU 可用。
- `unavailable`：dGPU 不可用，附带 `reason` 说明原因，例如“缺少 dGPU 资产”“dGPU 探针失败”。

dGPU 不可用时，节点仍可使用整卡部署；平台不会把不满足 dGPU 条件的实例静默改成不限制显存的实例来启动。
昇腾实测组合、CANN 和 runtime 细节见[昇腾矩阵](ascend-support-matrix.md)；PPU 未完成项见[开发台账](../dev/ledgers/ppu-support-gaps.md)。


## dGPU 能力对比

| 后端 | 显存限制 | 算力语义 | 修改显存配额 | 行为探针 |
|---|---|---|---|---|
| NVIDIA HAMi-core | 硬显存上限 | 平台不开放算力上限；CUDA graph、cuBLAS 不受 HAMi 发射限流约束 | 支持安全在线调整；引擎可能仍需重启才能利用新额度 | 依赖内嵌资产校验与准入检查，豁免行为探针 |
| 昇腾 vCANN | 显存配额，依赖宿主 device-share 权限 | 受 vCANN 与宿主组合影响，不据此承诺固定吞吐比例 | 重启实例生效 | 必须逐卡通过 |
| PPU HGGC | 显存配额 | 繁忙时加权 QoS，空闲时可借用余量；非硬 SM 分区 | 重启实例生效 | 必须逐卡通过；当前仅 Docker |
| 海光 Z100 / K100_AI 原生 vdev（实验性） | 原生 vdev 显存配额；每实例一张卡 | CU 随显存比例分配，不支持独立设置算力配额 | 重启实例生效，并按新显存配额重建 vdev | 必须逐卡通过；仅 x86_64 + Docker |

在线调整的安全条件与待重启原因见[调整 dGPU 配额](../how-to/dgpu.md#调整-dgpu-配额)。
表中能力不代表任意推理镜像均已验证；节点是否可用仍以该卡的 `status` / `reason` 为准。

## PPU 设备身份与 dGPU 探针

PPU 使用 `ppu-smi` 查询并经设备节点校验的身份，按选卡顺序分别注入两份数据：`CUDA_VISIBLE_DEVICES` 为计算程序使用的数字设备编号，`GPUWORKS_PPU_DEVICE_UUIDS` 为探针和 HGML 配额库使用的完整 UUID 列表。Docker/nerdctl 的数字编号来自过滤后的设备视图，udocker/runveil 使用宿主编号；两份列表的顺序一致。vLLM 和 SGLang 共用数字选择，不增加引擎判断。HGML 复用 UUID 顺序定位配额与显存账本；新 UUID 变量缺省时兼容旧 Agent 的设备选择变量。

内嵌探针 `gpuworks/partition-probe:v4` 同时检查目标 UUID 和显存配额。缺失、无效、重复或不可查询的身份，以及只有其他卡显示配额的情况都不能通过。升级 agent 后需重新创建已有工作负载，新的设备选择与切分库才会生效；runveil 的设备可见性仍不等于内核设备隔离。

## 海光整卡驱动库

海光整卡工作负载通过结构化挂载将宿主 `/opt/hyhal` 传入容器，Docker、nerdctl 与 udocker/runveil 共用该挂载；例如 DTK 26.04 的 PyTorch 需要其中的 `librocm_smi64.so.2`。节点缺少该目录时，设备注入会提前报错。容器继续使用镜像自带的 DTK 与库搜索路径，升级 Agent 后需重新创建工作负载以应用挂载修复。挂载声明为只读；PRoot 的绑定沿用宿主运行账户权限，不提供内核级只读隔离。

## 海光 Z100 / K100_AI dGPU

Z100 和明确识别为 `K100AI` 的 K100_AI 使用 `hygon-dgpu` 后端；`K100_AI`、`K100 AI` 是同一 SKU 的别名。只报告系列 `K100` 时仍不能据此启用 dGPU。agent 通过 `hy-smi virtual -d N --show-device-info` 读取所选物理卡的 CU 总数，再按显存比例向上取整分配 CU；查询失败或结果不完整时拒绝创建 vdev，不回退到固定 CU 数。启用前，在节点构建并安装可选的 `gpuworks-hygon-vdev-helper`（构建步骤见源码的 `build/runtime-assets/hygon-vdev-helper/README.md`），并提供宿主 DTK、HYHAL 和 DCGM 库。agent 本身仍是静态二进制。仅 x86_64 + Docker 路径受支持；其他型号不会自动获得此能力。

每个 dGPU 实例只能使用一张受支持的海光卡。请求显存向上取整到 MiB 后必须大于零且严格小于该卡物理显存；CU 数按 `ceil(配额 MiB × 物理卡 CU 总数 / 物理显存 MiB)` 分配，不提供独立算力配额，也不承诺同等比例的推理吞吐。修改配额后需重启实例，释放旧 vdev 后按新配额重新创建。

当前兼容性配置列出 DTK 26.04.x × x86_64，但该配置尚未标记为整套真机验证通过，版本信息仅作兼容性提示，不能替代逐卡探针。

Z100 和 K100_AI 的容器配置均使用与原生 ID 一致的 `/etc/vdev/docker/vdev<ID>.conf`，且只挂载一份 Docker 配置。当前两种型号的真机验证表明，将非零原生 ID 改名为 `vdev0.conf` 会导致 HIP 初始化失败；旧版 HYHAL 1.4 环境尚未重新验证原生文件名，仍须通过逐卡探针。轻量探针使用私有 `/var/log/hylog` tmpfs，并按宿主实际存在的 SONAME 补挂 tinfo5/6。

逐卡探针必须确认 HIP 看到请求的显存配额、可分配 1 MiB，并拒绝超额 1 MiB 的分配。探针通过后才能调度 dGPU 实例。推理容器使用自己的 DTK；宿主 DTK 与 ABI 补充库只供轻量探针使用。内置 Qwen3-0.6B 的 `bf16-vllm-hygon-z100` spec 使用经过 Z100 验证的 SourceFind 镜像。

普通用户可以使用 root 预先创建的 vdev。镜像必须提供运行 UID 对应的用户记录和可写的 HYHAL 日志目录；工作负载必须能读取配置快照、DTK/HYHAL 库，并访问 `/dev/kfd`、`/dev/mkfd` 和所选 DRM 设备；宿主 DTK 若通过符号链接落在 `/root` 下，普通用户仍可能被目录权限阻挡。K100_AI 的 HYHAL 1.5.0 CLI 创建和删除 vdev 均要求 root，因此非 root 工作负载可用不等于整个 agent 的 vdev 生命周期可在非 root 下运行。无需给推理容器 root 来满足宿主侧的生命周期权限。

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
