# ADR 0077: 托管的可写目录：模板声明 `dirs`，与 KV 磁盘层同一机制

日期：2026-11-19 · 状态：已接受

## 背景

有的配方启动时要一块可写的主机目录：多机推理的会话库与溢出区、首次启动推导出来以后复用的缓存（引擎编译缓存、快照目录，todo T59）、日志。这块目录要在实例停止、重启、多机组重组之后还在，要在部署删除时清掉，大小要计入调度。此前模板不能声明挂载（`docker_args` 禁 `-v`），部署的 `extra_mounts` 要运维手填主机路径，容器可写层在实例重建时丢失。平台已有一种托管的可写主机目录：KV 磁盘层（`MountPurposeKVDisk`），但它是缓存，只服务 vLLM 的磁盘层。

## 决策

1. **一个概念：托管的可写目录**。agent 在运行时挂载根下创建、以运行时挂载属主读写挂载、server 只给容器路径与以字节计的上限（`SizeHint`）而不给主机路径、调度在同一块盘上按上限预留的目录。它有两种，按生命周期分：

   | | 模板的 `dirs` | KV 磁盘层 |
   |---|---|---|
   | 谁声明 | 推理模板 spec | 部署参数（`shared_kv_dir`、`kv_disk_limit_gib`） |
   | 挂载用途 | `managed-dir` | `kv-disk` |
   | 主机目录 | `<根>/managed-dir/<部署 id>-<创建秒>/<name>` | `<根>/kv-disk/<部署 id>` |
   | 保留 | 到部署删除（`retain: deployment`） | 到该节点上最后一个实例离开 |
   | 内容 | 状态，不修剪 | 缓存，清扫器修剪到上限 |
   | 上限 | `limit_bytes` | 声明值，否则权重的 2 倍 |

   两者共用 `RuntimeMountManager` 的目录准备与填入（`setManagedMountSource`），准入一起计（`deploy.ManagedDiskBytes`）。

2. **spec 字段 `dirs`**：`[{name, mount_path, limit_bytes, retain}]`，至多 8 项。大小用字节，与 spec 已有的 `weight_bytes`、`shm_bytes`、组件 `weight_bytes` 一致（GiB 只出现在部署 API 的 `vram_limit_gib`、`kv_disk_limit_gib`，不在 spec 里）；容器路径叫 `mount_path`，与 `components[].mount_path` 一致。`name` 是短的小写标签，`retain` 目前只有 `deployment`（缺省时导入写回）。一个具名类型 `deploy.ManagedDir` 贯穿 YAML/JSON、`inference_specs.dirs` 列（整体读写的 JSON 快照）与部署的目录选择快照；快照里没有这个键就是没有声明。
3. **启动契约**：每项变成一个 `MountPurposeManagedDir` 挂载，带 `ManagedDirRef{group, name}`；group 是 `<部署 id>-<部署创建秒>`（`proto.ManagedDirGroupKey`）。各运行时（docker、nerdctl、runveil、Kubernetes、主机进程）都按普通挂载处理。同一节点上同一部署的实例共用一份。
4. **释放由 server 告知**：部署行删除后，server 向所有宣告 `mount.managed_dirs` 的在线 agent 发 `StateUpdate.ReleasedDirDeployments`；agent 在 hello 里列出自己持有的组（`Hello.ManagedDirGroups`），server 的重放回答其中部署已不存在、或同号部署创建时间不同的组（`DesiredState.ReleasedDirGroups`）。agent 只删名字是组键、且没有期望实例挂载的组。
5. **准入**：节点上报托管目录所在磁盘的空闲空间（`PathUsage` 种类 `managed_dirs`），并标出与它同一文件系统的其他上报路径（`shares_disk_with`）。调度要求空闲空间 ≥ `dirs` 上限之和 + KV 磁盘层上限 + 磁盘保留空间（存储设置的保留值，未设时 `DefaultReserveFreeBytes`），同盘时再加待下载的权重与待解压的镜像；不足时以 `insufficient_managed_dir_disk` 拒绝。未上报这块盘的节点不检查。
6. **T59 的缓存目录就是一项 `dirs`**：引擎编译缓存、TensorFold 快照目录写成 `dirs: [{name: compile-cache, mount_path: /cache, limit_bytes: …}]`，由启动参数或 `env` 指向该路径。它因此跨实例重建保留，不再像 T59 早先设想的那样留在容器可写层、随容器丢失。
7. **不新增启动变量**：目录在容器里的路径就是声明的 `mount_path`，配方直接写它；需要按 rank 分开时用已有的 `${GPUWORKS_NODE_RANK}` 拼子目录。今后若要暴露（例如 `${GPUWORKS_DIR_<NAME>}`），沿用 `${GPUWORKS_*}` 启动变量的写法与展开规则。

## 各声明的分工

`dirs` 与权重、组件、资产、KV 磁盘层、部署的 `extra_mounts` 各管一种容器内事实：只读的权重与资产随模型或模板版本，可写的 `dirs` 随部署，KV 磁盘层随节点上的实例，`extra_mounts` 不由平台管理。完整的一张表（含启动变量、系统层环境变量与平台文件）在 [推理目录语法参考的「模板与平台的分工」](../reference/inference-catalog-spec.md#模板与平台的分工)，这里不再另列一份。

- **一个容器路径空间**：`dirs` 不另立路径规则。模板自己的容器路径（主模型挂载点、组件 `mount_path`、资产 `unpack_to`、dir 的 `mount_path`）在导入时用同一条重叠规则（`proto.ContainerPathsOverlap`）互查，冲突报 `CATALOG_MOUNT_PATH_CONFLICT`；启动解析时所有挂载（含 `extra_mounts`、KV 磁盘层、计算网络文件）再查一遍，同一目标报 `MOUNT_TARGET_DUPLICATE`、嵌套报 `MOUNT_TARGET_NESTED`。
- **旧 agent 按能力拒绝**：托管目录只有节点上的 agent 能建，所以用到它的启动只放到宣告 `mount.managed_dirs` 的节点上；planning 从解析出的启动判断它用了哪些 agent 侧设施，旧 agent 的节点以 `agent_capability_missing`（参数 `capability`、`node`）拒绝，部署显示原因并等待。这是所有 agent 侧设施共用的规则，而不是让旧 agent 收到一个没有主机来源的挂载再启动失败。

## 取舍

- **KV 磁盘层不改写成一项 `dirs`**：它的开关与大小属于部署（运维按部署打开、上限默认按权重推导），不是模板的事实；它是缓存，要修剪、节点上没有实例就删，而 `dirs` 是状态，要跨实例保留到部署删除。把两者写成同一个 spec 字段，要给 `dirs` 加「修剪」「按节点保留」「大小从权重推导」三个开关，模板作者就得懂 KV 缓存的实现。现在的做法是同一机制、两种生命周期，代码路径共用，表里写清差别。现有 KV 磁盘目录的位置不变，避免迁移节点上的数据。
- **KV 磁盘层的上限进入托管目录磁盘的准入**：两者在同一个运行时挂载根下，只算一种会让另一种把盘写满。代价是开了 KV 磁盘层的部署在上报 `managed_dirs` 的节点上多一项检查。
- **节点自己知道持有哪些组，server 不记录**：「哪些节点上有某部署的目录」不能从实例推出，记一张表就是新的持久化事实，还要与节点磁盘对账。改由节点在 hello 上报、server 回答，删除时向所有在线节点广播一次（消息很小，删除部署是低频动作）。代价是广播落空的组要等该节点下次连接才删。
- **组键带创建时间**：SQLite 自增主键在一个库内不复用，但重装 server、恢复备份后同号部署会出现；只用 id 会把旧部署的状态交给新部署。创建秒足够区分，可由部署行直接得到，不新增列。
- **`retain` 只有 `deployment`**：`instance`（随实例删）等于容器可写层，`node`（跨部署）没有归属；先留出字段，不实现。
- **不设配额、不测用量**：KV 磁盘用部署 id 作 XFS 项目号，托管目录若也用会共享额度；另开号段要管理分配。逐目录统计用量要像 KV 清扫器那样定期遍历，几百 GiB 的状态目录代价不小。上限先只用于准入。
- **准入不按已用量折减**：重新调度到已有目录的节点时仍按全量预留；已写数据已在空闲空间里扣掉，偏保守。
- **同盘判定是 agent 的事**：只有 agent 能比较设备号；server 只读 `shares_disk_with`，与模型目录同盘时把权重整体计入（即使部署的存储在别处），宁可多算。

## 后果

- 新列 `inference_specs.dirs`（迁移 231），模板 DTO 的 `dirs`，部署快照的 `dirs`，评估预览的挂载里出现 `purpose: managed-dir`。
- 协议：`MountSpec.managed_dir`、`Hello.managed_dir_groups`、`DesiredState.released_dir_groups`、`StateUpdate.released_dir_deployments`、`PathUsage.shares_disk_with`，agent 能力 `mount.managed_dirs`，失败码 `MANAGED_DIR_PREPARE_FAILED`，调度原因 `insufficient_managed_dir_disk` 与 `agent_capability_missing`，导入错误码 `CATALOG_MOUNT_PATH_CONFLICT`。
- 旧 agent 的节点不接声明了 `dirs` 的启动，节点的 agent 升级后才放得上。
- 语法与行为见 [推理目录 model.yaml 语法参考](../reference/inference-catalog-spec.md#dirs托管的可写目录)。
