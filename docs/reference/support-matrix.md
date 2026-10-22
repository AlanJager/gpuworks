# 厂商支持矩阵

| 厂商/框架 | 探测 | 整卡部署 | dGPU | 拓扑/备注 |
|---|---|---|---|---|
| NVIDIA / CUDA | nvidia-smi、sysfs | Docker、runveil、nerdctl | HAMi libvgpu.so（x86_64，glibc ≥ 2.17） | 支持 CUDA/NCCL 拓扑；驱动库由宿主机提供 |
| 昇腾 / CANN | npu-smi、驱动和 CANN 事实 | Docker + Ascend runtime；部分环境 runveil | vCANN；能力取决于宿主机 device-share 权限 | 外层/内层 CANN 版本不能混为一个结论 |
| PPU / HGGC | ppu-smi | provider/runtime 依赖 | HGGC 显存与加权 QoS | 20/80 长负载已证明繁忙时吞吐权重，不是硬 SM 分区 |
| 海光 / DTK | hy-smi | 由目录 spec 和 runtime 决定 | 需对应资产与真机证据 | 不凭统一模板推断支持 |
| Apple / Metal | sysctl、ioreg（macOS 15+，Apple silicon） | host（引擎为宿主机进程，镜像是 darwin 环境包） | 无；一台 Mac 按统一内存同时跑多个实例 | 1 张统一内存卡，显存为 Metal 工作集；不支持引擎休眠、多机、KV 磁盘层；见 [ADR 0044](../adr/0044-mac-engines-as-host-processes.md) |

能力状态是 ready 或 unavailable（不可用时附带 reason，如"缺少 dGPU 资产""dGPU 探针失败"等），
由 server 的 partition capability 推导。节点仍可使用整卡部署；不满足 dGPU 条件时不会静默启动
无限显存实例。昇腾实测组合、CANN 和 runtime 细节见昇腾矩阵；PPU 未完成项见开发台账。


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
