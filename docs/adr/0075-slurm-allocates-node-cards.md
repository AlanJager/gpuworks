# ADR 0075: Slurm 计算节点上卡归 Slurm，实例用占位作业借卡，借到哪张用哪张

日期：2026-11-15 · 状态：已接受

## 背景

用户已有 Slurm 集群，希望不改集群就把空闲的卡用来跑推理服务。Slurm 管排队、分卡和到点收回，不管常驻服务、对外暴露和健康检查；gpuworks 正相反。两者在同一台机器上时，卡只能有一个分配者，否则 Slurm 会把 gpuworks 正在用的卡分给作业，或者 gpuworks 把作业的卡拿去部署。

Kubernetes 接入（[ADR 0052](0052-kubernetes-nodes.md)）已经给出同类问题的答案：节点上常驻 agent，卡归集群的分配者，平台要用时借。Slurm 与 kubelet 的差别在于：`--gres=gpu:N` 只能要数量，不能指定是哪张卡；账户与 QOS 可以限制卡数、作业数和卡时；作业由 `scancel`、时限和抢占结束。

## 决策

1. **节点就是物理机，agent 常驻**：agent 由第 8 条的集群条目装到每台 GPU 计算节点（systemd 服务）。agent 发现 `scontrol` 认得本机（节点名取条目登记时 slurmd 的名字；不经条目安装的 agent 默认短主机名，`GPUWORKS_SLURM_NODE` 覆盖）且本机有 `gpu` GRES 时，进入 Slurm 模式（`GPUWORKS_SLURM=auto|on|off`，默认 auto）。Slurm 不分配 GPU 的节点，卡仍是平台的。
2. **其他作业的卡报为外部占用**：agent 每轮用 `squeue -w <节点>` 列出本机作业，读每个作业 `scontrol -d -o show job` 中本节点的 `GRES=…(IDX:…)`；`IDX` 是 Slurm 对本机设备列表的位置，按 `slurmd -G` 的设备文件（configless 节点带上运行中 slurmd 的 `-Z`、`--conf`、`--conf-server` 参数；都拿不到时按设备 minor 顺序）经 `/proc/driver/nvidia/gpus/*/information` 的 `Device Minor` 对到卡。这些卡经已有的外部占用路径上报，原因是 `Slurm job <id> (<user>)`；调度器不改，与 ADR 0052 第 3 条相同。
3. **实例启动前借卡，借到哪张用哪张**：engine 在启动实例前经可选的 `CardLeaser` 借卡。Slurm 的实现为实例提交一个作业 `sbatch -w <节点> --gres=gpu:N -J gpuworks-hold-<实例> --wrap 'exec sleep infinity'`（默认 `-c 1 --mem=1G --no-requeue`，没指定分区时用本节点所在的分区；条目的 sbatch 参数追加账户、分区、QOS、时限等，不经条目的 agent 用 `GPUWORKS_SLURM_SBATCH_ARGS`），等它 RUNNING，从作业的 `IDX` 读出实际的卡。要的卡已被本 agent 的占位作业持有时（dGPU 共卡、agent 重启后按作业名找回）直接用，不再借。PENDING 时把 `Reason` 作为实例进度上报，超过排队超时（条目的设置，默认 10 分钟；不经条目的 agent 用 `GPUWORKS_SLURM_PENDING_TIMEOUT`）取消并判启动失败。
4. **server 事后改绑**：借到的卡与调度器选的不同时，agent 用借到的卡启动，并在 `InstanceStatus.gpu_idents` 上报；server 按卡身份改写实例的设备序号、卡身份与期望状态（与探测重编号时的改写同形），账本随之计在真正用着的卡上。server 在 hello 里宣告 `instance.card_rebind`；不宣告时，卡不同则启动失败。借到的卡型号或显存与所选不同时同样失败，因为调度是按所选的卡估算的。
5. **归还与收回**：占位作业的卡没有实例使用超过空闲归还时间（条目的设置，默认 60 秒；不经条目的 agent 用 `GPUWORKS_SLURM_IDLE_RETURN`）后 `scancel`；没有人在等的 PENDING 占位作业（重启前留下的、启动已放弃的）直接取消。占位作业被外部结束（`scancel`、时限、抢占）时，用着它的卡的实例停止并判失败，消息带作业号与 Slurm 给的状态，由 server 重新调度。
6. **云上 HPC 集群的计算节点**：阿里云 E-HPC 的计算节点在云账号里识别（ECS 标签 `ehpc.aliyun.com`，集群名与调度器来自 `ehpc:ListClusters`），整个集群作为一个集群条目接入（第 8 条），平台经云助手在 Slurm 管控节点上替管理员执行接入脚本，并为初始节点补上 `gres.conf` 的行。节点的增减归集群的队列，平台不开关机、不释放、不改规格（`PROVIDER_INSTANCE_IN_HPC_CLUSTER`）；调度器不是 Slurm 的集群不提供接入，否则 agent 与该调度器的作业会用同一批卡。
7. **只经 CLI**：与运行时驱动一样只调用 `scontrol`、`squeue`、`sbatch`、`scancel`、`slurmd`，不依赖 slurmrestd，也不链接 Slurm 的库。
8. **以集群条目接入，健康检查是每个节点的引导程序**：一个 Slurm 集群是「外部平台」里的一个条目，带一个接入令牌（对标 Kubernetes 集群，[ADR 0052](0052-kubernetes-nodes.md)），不逐台安装 agent。管理员在 Slurm 管控节点上以 root 执行一次接入命令，它把 `HealthCheckProgram` 写进 `slurm.conf`（原有的健康检查串接保留）。slurmd 在每台计算节点上以 root 运行它：节点注册时、`scontrol reconfigure` 时、之后每个周期，所以现有节点和以后扩容的节点都自动装上 agent，这是 Slurm 里 DaemonSet 的对等物。有 NVIDIA GPU 且 agent 未运行时，它在后台装 agent 并以条目令牌登记（按条目与 Slurm 节点名幂等，首个节点把令牌绑定到 Slurm 的 `ClusterName`）；没有 GPU 的节点什么也不做；它永远不 drain 节点。
   - **agent 默认以 root 运行**，可在条目里改成普通账号：agent 的全部功能只在 root 下充分验证，普通账号的已知限制（configless 的卡序、本机 docker 组、目录权限）在 root 下不存在，而 docker 组本身等同 root，降权收益有限。占位作业记在哪个账号单独设置：root 的 agent 用 `sbatch --uid=<账号>` 提交，记账与 `squeue` 里就是那个账号。
   - **否决的做法**：在每台节点上逐台安装（扩容的节点要再装一次，体验也不能对标 Kubernetes）；用 E-HPC 的集群后处理脚本在节点上装（试过，在我们的环境里没有执行）；平台定期同步集群并替新节点装 agent（产品负责人否决）。健康检查实测（E-HPC configless）：slurm.conf 在扩容、缩容、更新队列后不被 E-HPC 改写，新节点注册时就执行，由它拉起的服务不受 slurmd 重启影响。
   - **原有健康检查的语义保留**：我们的块要在任何节点状态下装 agent，所以 `HealthCheckNodeState` 是 `ANY`（原来有 `CYCLE` 时 `ANY,CYCLE`，继续错开各节点）；原来的程序、节点状态、周期取 slurmctld 正在用的值（`scontrol show config`，设置可能在 `Include` 的文件里），程序与状态记在脚本目录的 `chained`，`check.sh` 只在节点处于原来要求的状态时运行它（`IDLE`、`NONDRAINED_IDLE`、`ALLOC`、`MIXED`，按 `scontrol` 查到的本机状态；查不到时照常运行）。这是近似：Slurm 自己按状态挑节点，这里是节点运行时自查。reconfigure 之后再问一次 slurmctld 是否在用我们的程序，不是就标为「有要手工处理的」。setup 只在运行 slurmctld 的机器上执行，在 configless 的 slurmd 副本上不执行。
   - **后台安装先随机等待最多半个周期（不超过 300 秒，安装进程限时 30 分钟）**再向 server 请求，免得整个集群同时下载 agent；安装脚本下载到 root 专有的临时文件后运行，退出码如实记入日志。agent 以普通账号运行时，健康检查（root）不运行、不写穿该账号拥有的目录里的任何东西。
   - 条目的节点名单按 `sinfo -a` 记全部节点、只有带 `gpu` GRES 的才要求 agent：没有 GRES 的 GPU 机器、隐藏分区里的节点仍在集群里，不会被当作离开了集群而清理；状态与原因的变化不重写整表。
   - 令牌放在脚本目录里仅 root 可读的文件中（健康检查脚本任何用户可读，不含令牌）。平台替 E-HPC 执行接入时，云助手的命令只带一次性地址，令牌只在下载到的脚本正文里。

## 取舍

- **借 N 张，不借全部再退**：ADR 0052 的普通 device plugin 先借全部空闲卡、留下要的、退回其余。Slurm 上有额度时（`GrpTRES`、`MaxTRESPerUser`、`MaxJobsPerUser`）全借会借不全、借错卡，排队中的占位作业还会在额度空出来后突然起来占卡；每卡一个作业也很快碰到作业数上限。代价是借到后才知道是哪张卡，因此要第 4 条的改绑，拓扑敏感的部署要靠站点给 `--gres-flags=enforce-binding` 一类参数。
- **一个实例一个作业，不是一卡一个作业**：作业数少，额度好算；代价是同一作业的卡只能一起还。
- **引擎在作业外运行**：形态 A 的引擎用节点原有的运行时（docker），不在作业的 cgroup 里，所以作业的 CPU 与内存申请只是记账（默认 1 核 1G，由管理员按需调大），设备隔离靠 agent 只把借到的卡交给容器。由 agent 跟踪作业结束并停实例，不靠作业脚本。agent 跑在作业里、引擎经 `srun --overlap` 进作业的形态（没有 root 的集群、Slurm on Kubernetes）不在本决策内。
- **只认 NVIDIA**：设备位置到卡的对应只实现了 `/dev/nvidiaN`；其他厂商的节点即使 Slurm 分配 GPU，也不进入 Slurm 模式（`on` 时记警告）。

## 后果

- 新包 `internal/agent/slurm`；engine 的 `CardLeaser`、`InstanceCards`、`LeaseLost`。
- 协议：`InstanceStatus.gpu_idents`，server 能力 `instance.card_rebind`。借卡不新增持久化事实（改写的是实例已有的 `GPUIndexes`、`GPUIdents` 与期望状态）。
- 云实例记录多四列 `hpc_cluster_id`、`hpc_cluster_name`、`hpc_scheduler`、`hpc_scheduler_version`（每次同步覆盖），账号的实例列表不含这些节点（同托管 Kubernetes 集群的节点：集群的节点不是实例，不计入账号的实例数），账号下的 HPC 集群由 `GET /providers/{id}/hpc-clusters` 实时列出。
- 经集群条目安装的 agent，设置来自条目（登记时和每次上报时由 server 下发，不进 agent 配置文件）；不经条目安装的 agent 用环境变量。
- 额度预检、按剩余时限接力、作业内 agent、OpenPBS 等调度器未做，见研究记录。
