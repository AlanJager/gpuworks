# 昇腾支持矩阵

更新日期：2026-10-24。这里把三件容易混淆的东西分开写：

- **外层 CANN**：节点或云市场租用镜像提供的 toolkit。自有裸机可能只有驱动，记为 `driver-only`，不等于没有容器 runtime。
- **内层 CANN**：GPUWorks 启动的业务镜像内的 CANN 用户态二进制接口（userspace ABI）；它影响业务镜像能否运行，但不参与 `libvruntime.so` 资产选择。
- **runtime（容器运行时）**：Docker、nerdctl 或 runveil（`udocker` driver）。Gitee 的租机镜像只是外层环境，不是业务镜像；表中的 `udocker` 场景通过 PRoot 在外层容器中运行业务容器（container-in-container）。下表中标 `udocker` 的结果是在旧的 udocker CLI 启动器下验证的；runveil 的实测单独列出。

## 当前实测矩阵

| 环境 | 架构 / NPU | 外层 CANN | 内层 CANN / runtime | 已通过的最高层级 | 结论 |
|---|---|---:|---|---|---|
| Gitee x86_64 | x86_64 / 910B2C | 9.0 | 9.0 / udocker | 平台整卡部署 + `/v1/chat/completions` | 支持；测试显式断言 `runtime_driver=udocker` |
| Gitee x86_64 | x86_64 / 910B2C | 8.5.1 | 8.5 / udocker | 平台部署到 healthy；dGPU 能力探针正确返回云侧拒绝 | 外 8.5 + 内 8.5 可启动；尚缺一份可重复 CI 的成功 chat 证据 |
| Gitee ARM | arm64 / 910B1 | 9.0 | 9.0 / udocker | 平台整卡部署 + `/v1/chat/completions` | 支持；真实预取 arm64 manifest/layers |
| Gitee ARM | arm64 / 910B1 | 9.0 | 8.5 / udocker | 容器启动到 CANN FE | 旧 PRoot 下未通过；`platform_config` realpath 失败。ARM64 `faccessat2` 已修复，该云端组合尚未复测 |
| Gitee ARM 目录项 | arm64 / 910B1 | 8.5 | 未进入内层 | 租机前置校验 | 云市场实际返回“镜像和主机不匹配”，不能作为 CI fixture；这不等于外 8.5 + 内 8.5 已被判定不兼容 |
| 自有池 Docker 成员 | arm64 / 910B4 | 9.0/驱动 | 9.0 / Docker | 平台 API 双副本同卡 dGPU、真实推理/chat、超配拒绝、整卡 chat | 支持；完整平台 dGPU 主验收 |
| 自有池 nerdctl 成员 | arm64 / 910B4 | driver-only | 内嵌 probe / nerdctl | 生产路径加载 2.5 MiB 行为探针，`dsmi_get_hbm_info` 精确返回 4 GiB | 支持节点级 dGPU；无业务镜像、模型或推理引擎 |
| 自有裸机 runveil（物理卡号 2–7） | arm64 / 910B4 | driver-only，26.0.rc1 | 内嵌 probe v2 / runveil | 生产 Engine + Runner 在物理卡 7 上返回 4 GiB；关闭 DSMI 钩子返回整卡 32 GiB 并判失败 | 节点级 dGPU 探针通过；旧 v1 可复现 `rc=87 total_kb=0` |
| 同一自有裸机 runveil | arm64 / 910B4 | driver-only，26.0.rc1 | 8.5.1 / vLLM 0.18.0 / runveil | 临时兼容对照通过 8 GiB 配额、9 GiB 分配拒绝及后续真实 chat（HTTP 200） | 默认路径仍未通过。对照额外绑定容器标记，并仅将 TSD 对不存在的卡 0 的芯片型号查询重试到真实卡 7；此诊断 hook 未进入 agent 或镜像，不作为默认支持 |
| 同一自有裸机 runveil | arm64 / 910B4 | driver-only，26.0.rc1 | 9.0 / vLLM 0.20.2 / runveil | 生产 Engine + runveil Driver 自动选卡，Qwen3-0.6B、8 GiB 配额，真实 chat 返回 HTTP 200 | 通过单实例 dGPU 推理；容器报告 8589934592 bytes，9 GiB 申请被拒绝，随后 chat 返回 `Hello! 😊`。本次未走平台 API/调度/网关 |

“外层 8.5、内层 8.5 已测过”指上表的 Gitee x86_64 环境：已有真实平台部署到 healthy 的现场记录，但当前仓库没有保留成功 chat 响应，因此矩阵不会把它写成完整对话通过。下一步应在现有 x86_64/CANN8.5 租机用例内复用同一实例补 chat，避免增加一次租机。

## 覆盖层级

1. **探测**：架构、NPU、driver/toolkit、runtime 和dGPU 能力原因。
2. **设备注入**：内层容器可见指定的物理 NPU。
3. **dGPU 行为探针**：走生产 `Engine` 生成 vCANN 配置并启动真实 runtime；内嵌 2.5 MiB `/probe` 调用 `dsmi_get_hbm_info`，断言看到的总显存恰好是 4 GiB。该层刻意不加载业务镜像或模型。
4. **平台 dGPU 推理**：必须经 GPUWorks API 创建部署和实例，验证同卡多副本、配额及真实推理。
5. **完整整卡对话**：必须经平台 API 部署，并从 `/v1/chat/completions` 得到真实回复。

自有池覆盖 1–5；Gitee 因租用容器无权执行 `npu-smi set -t device-share`，只能覆盖探测和整卡 container-in-container 对话，不能把云侧权限拒绝伪装成 dGPU 通过。

## 已修复的误判

### 行为探针固定查询逻辑设备 0

旧探针固定调用 `dsmi_get_hbm_info(0)`。PRoot 不建立 OCI 设备隔离，DSMI 的编号取决于驱动所见的设备：可能保留宿主编号，也可能将物理卡 5、7 映射为逻辑卡 0、1。固定查 0 可能查询不存在的设备，也可能查询另一张卡；直接使用物理卡号同样可能出错。因此不能仅凭容器运行时名称决定查询编号。

agent 现在通过 `GPUWORKS_PARTITION_PROBE_DEVICE` 传入所选物理卡号；探针先用 `dsmi_get_logicid_from_phyid` 解析当前运行环境中的逻辑号，再查询该设备的 HBM。缺少映射接口、映射失败或显存不等于配额均返回失败，不回退到 0 号卡。诊断输出同时记录物理号和逻辑号。

内嵌探针镜像升级为 `gpuworks/partition-probe:v2`，避免升级 agent 后继续命中已缓存的 v1。该修复针对节点级行为探针；真实推理的支持范围仍以实测矩阵为准。

### runveil 推理设备选择与 ARM64 路径解析

runveil 保留宿主设备视图，不能沿用 Docker 的紧凑设备号。agent 现在结合 `npu-smi info` 的物理卡/芯片地址与 `npu-smi info -m` 的 `Chip Logic ID`，生成 `ASCEND_RT_VISIBLE_DEVICES` 和 `NPU_VISIBLE_DEVICES`；缺失或冲突的映射会阻止启动，不回退到 0。真机验证了裸机物理 7→逻辑 7，以及直通 VM 物理 7→逻辑 1 的生产注入结果。

此前 ARM64 PRoot 的系统调用表缺少 `faccessat2`（439），导致 glibc `realpath()` 对目录的检查使用宿主路径，误报 `ENOENT`。CANN 因而找不到 platform 配置或算子注册库；CANN 9.0 表现为 `ZerosLike` / `aclnnInplaceZero` 的 `361001`。该错误在关闭 dGPU 的对照中也能复现，不能据此判定 vCANN 不支持 runveil。

已补齐系统调用映射并重建 ARM64 内嵌 PRoot。构建门禁验证 guest bind 下的 `faccessat2`、相对 dirfd、缺失路径和带符号链接/父目录/尾斜杠的 `realpath`；原二进制失败，新二进制在 ARM64 真机通过。使用更新后的内嵌资产和自动设备映射，CANN 9.0 / vLLM 0.20.2 的单实例 dGPU 推理、超额分配拒绝及拒绝后的 chat 均通过。范围仍以上表为准。

CANN 8.5.1 另有 TSD 初始化编号顺序问题：runtime 把选中的逻辑卡 7 转成用户设备号 0 后，TSD 在映射回逻辑号前查询芯片型号，因此在缺少逻辑卡 0 的节点上返回 87，最终表现为 `TsdOpen` / `507033`。真实二进制反汇编及单次查询重试对照验证了这一原因；对照返回真实驱动数据，没有伪造设备启动成功。临时容器标记加该诊断 hook 后，Qwen3-0.6B 的配额检查与 chat 均通过，但生产路径仍需解决 DCMI 检测阻塞并修正 TSD 转换顺序。此实验未走平台 API/调度/网关，也未将临时兼容处理加入默认运行时。

### “没有 runtime”

历史 ARM fixture 报“节点没有可用的 docker/udocker 运行时”，不是 CANN 声明，也不是硬件不支持。该外层镜像的 Python 位于 `/root/miniconda3/bin/python3` 或 `/opt/conda/bin/python3`，不在非交互 shell 的默认 `PATH`，旧探测因此没有启用内嵌 udocker/PRoot。

现在探测会检查这些绝对路径，runtime 可正确识别为 `udocker`。这一探测现在只对 `GPUWORKS_UDOCKER_LAUNCHER=udocker` 有意义：默认的 runveil 启动器不需要 Python。

### `libvruntime.so` 只按 CPU 架构区分

历史上 CANN 8.5.1 和 9.0.0 目录中的同架构产物字节级相同：arm64 SHA256 均为 `3dbe727a…d3d49`，amd64 均为 `57a9641a…9d010`。它们来自同一固定 ubs-virt 源码和 patch，并不是两套运行时 ABI。

仓库现在只保留 `linux-arm64/libvruntime.so` 与 `linux-amd64/libvruntime.so` 各一份，作为各架构唯一的标准路径（canonical 路径）。运行时只接受这两个路径，不读取外层或内层 CANN 版本，也不存在跨 CANN 版本回退。

agent 升级并成功安装标准路径下的资产后，会精确删除旧的 `cann-8.5.1/` 与 `cann-9.0.0/` state 目录。CANN 版本只保留为实测环境记录和兼容警告。

节点行为探针通过只证明“节点 + runtime + 该 SO（共享库）”的 dGPU 链路可用，不自动证明任意业务镜像可用。业务镜像是否支持，应由使用该镜像用户态环境（userspace）的专用行为探针（image-specific probe）判断；现有版本矩阵在完成这层探针前只能标为 warning/unknown，不能作为支持断言。ARM 外 9 + 内 8.5 的 PRoot `platform_config` 失败正说明二者不能混为一个结论。

`PartitionProfile.Verified` 是代码中的验证标记，不能替代上面的真机矩阵：

- **定义位置**：`internal/accelerator/ascend.go` 的 profiles 表，按 backend/framework/版本前缀/架构硬编码的静态布尔位。
- **标记含义**：只表示该组合在代码里被标记为已在预置真机环境验证过。为 false 时，`partitioncap` 会追加“该运行时组合尚未经预置真机环境验证”提示。
- **维护方式**：随代码提交手动维护，不会随本文档的真机矩阵自动同步。例如 CANN 9.0.x × x86_64 已有完整 chat 记录，但该字段尚未标记为 Verified。
- **校验边界**：不表示对应 `libvruntime.so` 资产本身经过额外的构建/来源校验；该校验由 `build/runtime-assets/verify.sh` 独立完成。

支持结论以上面的真机矩阵为准。

## CI 归属

- `l4-gitee-ascend-partition-capability`：租用 x86_64/CANN8.5 实例，验证节点能力探测和已知的 device-share 限制，不下载业务模型。
- `l4-gitee-ascend-deploy-chat`：显式低频运行 x86_64/CANN9 和 arm64/CANN9 的整卡部署与真实对话。
- `l4-ascend-pool-partition-inference`：把 `.31/.32` 当能力池而非绑定节点；验证平台 API dGPU 推理/整卡对话，以及 nerdctl 设备注入和内嵌行为探针。
- nerdctl helper 是仓库内约 9 MiB 的 gzip arm64 资产。CI 和 GPU 节点不执行 `go build`；更新时只在 内部构建机（见 `secrets/test-environments.md`） 构建后签入。

失败调试默认先保留最后一个现场：云租机用 `GPUWORKS_CLOUD_KEEP_ON_FAILURE=1`，nerdctl helper 用 `GPUWORKS_KEEP_ON_FAILURE=1`。确认原因后按本次 run 的精确实例、容器和临时目录清理。

## 卡与芯片：识别与编号

每颗芯片是一个独立设备（自己的 `/dev/davinciN`、PCIe 功能、HBM 与健康状态），GPUWorks 按芯片上报与调度。
卡的布局按 `npu-smi info` 的表格判定，与 zstack-utility 的 `resolve_handler` 一致：

- **一卡一芯片（910B）**：每个 NPU 一行芯片、表头没有 `Phy-ID` 列。设备编号是 NPU ID。
- **一卡多芯片（910C）**：表头有 `Phy-ID` 列，或同一 NPU 下出现多个 Chip ID，或芯片名含 `910C`。
  910C 的芯片名列写的是 `Ascend910`（不带后缀），每个 NPU 两颗芯片（Chip 0/1）；GPUWorks 把这种卡的芯片记为型号 `910C`。
  初代 910 是同名的一卡一芯片卡，不会被认成 910C。设备编号是芯片的 Phy-ID；没有 Phy-ID 的芯片无法对应设备，不上报。

设备编号统一取 Phy-ID 的依据（2026-10-02 两台 910C 主机只读核实，记录见
`artifacts/archive/2026-10-02-gpu-identity/evidence/`）：

| 用途 | 编号 |
|---|---|
| `/dev/davinciN`、`ASCEND_VISIBLE_DEVICES`（Ascend Docker Runtime）、nerdctl `--device` | Phy-ID |
| `npu-smi info -t topo` 矩阵 | `Phy-ID<n>` 行列（芯片间 `SIO`、卡间 `HCCS_SW`，都按高速互联） |
| `hccn_tool -i` | Phy-ID（zstack-utility `fedf490a2` 同样改用物理 ID） |
| `npu-smi info -t health/board/device-share -i <npu> -c <chip>` | NPU ID + Chip ID，由 agent 从表格换算 |
| 容器内 `ASCEND_RT_VISIBLE_DEVICES` | 从 0 起的紧凑编号 |

`npu-smi info -m` 里 910C 的 Logic ID 与 Phy-ID 相同，且不随缺卡压缩（缺 NPU 3–6 的主机上 NPU 7 的两颗芯片仍是 14、15），
`/dev/davinci0..15` 全部存在，与 Phy-ID 一一对应（`Phy-ID = 2 × NPU ID + Chip ID`）。
910B 的 NPU ID 即 Phy-ID，因此一卡一芯片卡沿用 NPU ID。
