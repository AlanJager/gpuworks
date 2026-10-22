# 排障指南：按症状定位

读者假定第一次运维 GPUWorks。每节固定三段：**现象**、**可能原因**（按概率从高到低）、**怎么确认与处理**。
原因排序是经验判断，先试前面的。

## 先看哪里

| 看什么 | 在哪里 | 用来回答 |
|---|---|---|
| 节点状态 | 节点页徽标：在线 / 离线 / 连接中（`connecting`） | **只看 agent 连没连上**；加速卡的问题不改变它 |
| 加速卡状态 | 节点**详情页**里的「厂商工具未响应」标记（节点列表不显示），以及加速卡列表里每张卡的分配状态（可分配 / 不可分配等）、健康、归属与占用 | 这张卡能不能用、读数是不是旧的 |
| 节点详情 | 「最近拒绝」行、「带外恢复」卡片、「诊断」按钮（`POST /api/v1/nodes/{uuid}/doctor`，与 `gpuworks-agent doctor` 同一检查） | 为什么被拒、恢复到哪一步、环境是否健康 |
| 实例事件时间线 | 实例详情；API `GET /api/v1/instances/{uuid}/events`（部署级：`GET /api/v1/deployments/{id}/events`） | 卡在哪个状态和阶段、失败原因（`params.detail`） |
| 调度解释 | 部署页；实例响应的 `schedule.explain`；`POST /api/v1/evaluate` 响应的 `plan` | 为什么选这张卡、别的候选为什么被拒 |
| 日志页 | 按实例、级别、来源（`source=instance\|node\|agent\|server`）、关键字检索，见[监控与日志](monitor-and-logs.md) | 引擎输出、`CUDA out of memory` 之类 |
| server 日志 | `gpuworks install` 安装的：`/opt/gpuworks/server/logs/gpuworks-server.log`；直接 `serve`：`<data>/logs/gpuworks-server.log`；systemd 下 `journalctl -u gpuworks` | 握手失败、恢复轮次、代理错误 |
| agent 日志 | `<state>/agent.log`（安装默认 state 为 `/opt/gpuworks`，10 MB 轮转、保留一代 `agent.log.1`）；`systemctl status gpuworks-agent` | 连接、GPU 探测、拉取、启动 |
| 环境自检 | 节点上 `gpuworks-agent doctor` | 运行时、GPU、驱动、设备权限 |

实例状态取值：`pending`、`pulling`、`starting`、`optimizing`、`running`、`removing`、`stopped`、`failed`、
`pending-removal`。`pulling` 期间的阶段（phase）依次可能是 `image_verify`、`image_pull`、`image_extract`，
`image_pull_retry` 表示某个镜像来源失败后换下一个；`starting` 期间是 `container_prepare`、`container_launch`、
`health_check`。

管理 API 的错误形态是 `{"error": "...", "code": "...", "params": {...}}`，`code` 稳定，以 `code` 为准。

## 节点一直「连接中」或不上线

**现象**：节点页一直是「连接中」，或上线后反复变「离线」；agent 日志出现 `connection ended`、
`reconnecting` 或 `server rejected connection`。

**可能原因**

1. server 地址从节点不可达或填错。安装命令里的 `--server` 就是 agent 永远拨打的地址，装完记在 `<state>/gpuworks-agent.conf` 的 `SERVER`
   里（`systemctl cat gpuworks-agent` 看不到它，也看不到 token，单元里只有 `run --state <目录>`）；`install.sh`
   会先从该地址下载 agent 二进制（下载不了就直接失败），但装完不验证 agent 能否建立长连接。Server 发起连接的节点没有这一项：
   它们不连接 server，要检查的是 server 能否访问节点的监听端口（安全组、防火墙、端口映射）。
2. token 错误。节点被删除、重新生成 token（`POST /api/v1/nodes/{uuid}/token/regenerate` 会踢掉当前连接）
   后，旧 token 会被拒，reason 为 `bad_token`（detail 为 `node token was not recognized`；空 token 是
   `node token is required`）。
3. 反代或监听没有转发 `/api/v1/agent/connect` 的 WebSocket upgrade，或该地址不在 `listen.agent` 里
   （不承载的功能返回 404，见[配置参考](../reference/configuration.md)）。
4. 协议版本不匹配，reason 为 `proto_mismatch`（detail 形如 `server supports 1-1`），需要升级 agent。
5. `stale_session`：只出现在**数据通道**握手，表示它绑定的控制会话已过期。agent 只打 Debug 日志并退避
   重试，控制通道重连后自愈，见 [ADR 0024](../adr/0024-data-lane-no-ctrl-fallback.md)；持续出现才需要处理。

时钟偏差**不是** reject 原因：握手里没有时间戳校验，`server_dial` 的 HMAC 只签一次性 nonce，不含时间。
server_dial 节点的密钥不符时 agent 报 `invalid server_dial proof`。

**怎么确认与处理**

```bash
# 在节点上：退出码 0 成功、3 超时（10 秒）、4 被 server 拒绝、1 其他
gpuworks-agent smoke --state /opt/gpuworks   # 读 gpuworks-agent.conf；也可显式 --server http://<server>:8800 --token <node-token>
cat /opt/gpuworks/gpuworks-agent.conf     # 含 token，注意不要贴出去
tail -f /opt/gpuworks/agent.log
curl -s -H "Authorization: Bearer <jwt>" http://<server>:8800/api/v1/agent/rejects
```

- `GET /api/v1/agent/rejects` 返回最近被拒的连接：`token_prefix`（token 前 8 位）、`reason`、`remote`、`at`；
  只在内存里保留最近 64 条，重启即丢。`bad_token` 对应不到节点，所以只在这里和 server 日志
  （`agent session failed`）里可见；节点详情「最近拒绝」只记录 `proto_mismatch`。
- 收到 reject 后 agent 固定等 5 分钟才重连（普通断线从 1 秒翻倍退避到 30 秒）；修好后想立刻生效，`sudo systemctl restart gpuworks-agent`。接入方式见[接入节点](connect-nodes.md)。

## 节点掉线后的自愈与 recovery

**现象**：节点从「在线」变「离线」，节点详情出现「带外恢复」卡片。

**自愈顺序**：agent 自己重连 → server 对 server_dial 节点反向重拨 → server 经 SSH 带外恢复
（[ADR 0006](../adr/0006-out-of-band-recovery-ssh-credentials.md)）。`--stale-after`（默认 90 秒）无心跳就标离线。

**可能原因（恢复没有发生或没成功）**

1. 节点没有保存 SSH 凭据：自动恢复直接跳过，手动触发返回 `ssh credential is not configured`。
   凭据只有地址时用平台密钥登录；错误里出现 `has not authorized the platform key` 说明节点上没有平台公钥，按错误给出的命令授权。
2. 还在等待窗口内：距上次在线不足 `--recovery-grace-period`（2m），或退避未到（30s 起、每次 ×2、上限 10m）；
   server_dial 节点还要等 server 反拨退避到最大档（5 分钟）。
3. SSH 本身不可达（按不可达处理并持续退避）。平台目前不校验 SSH 主机密钥：云厂商会把释放的公网 IP 分给别的实例，按地址记住的旧密钥曾让卸载与恢复永远失败。
4. 同一阶段连续失败两次，阶段变 `recovery_blocked`，自动恢复停止，只能手动触发。
5. agent 进程在跑却没建立控制会话（错误文案 `agent is running but has not established a control-plane session`）：
   SSH 恢复不会重启或重装它，问题在第一节，去看 agent 日志。
   只有 unit 所指 agent 的 server（取自它的配置文件，较早安装的 agent 取命令行里的 `--server`）与本节点的 server 地址一致（或读不出，Server 发起连接的节点没有）时才算本节点的 agent。
6. 主机上的 `gpuworks-agent` unit 属于另一个 server（错误以 `foreign_agent:` 开头，括号里是那个 server 的地址）：
   所有安装共用这个 unit 名，后装的会覆盖先装的。SSH 恢复不会重启或重装它，以免把主机从另一个 server 抢走；
   确认那个 server 已不用这台主机后，在主机上 `systemctl stop gpuworks-agent`（unit 文件先备份），再手动触发恢复，
   恢复会用本节点的 token 重装 unit。已停止的他人 unit 不受此限，恢复不会重启它，直接重装。
7. 云实例已销毁：阶段 `stopped`，原因 `provider instance no longer exists`。经 API 释放成功或同步发现云端已无该实例时
   立即置为此阶段（节点此刻可能还在线，agent 重新连上会清除）；节点本身保留，确认不再需要后手动删除。

**怎么确认与处理**

- 节点详情「带外恢复」卡片显示阶段、尝试次数、上次探测时间和最后错误（字段 `recovery_phase`、
  `recovery_attempts`、`last_probe_at`、`last_recovery_error`）。只有 `recovery_phase` 非空时才出现。
- 写入 SSH 凭据：`PUT /api/v1/nodes/{uuid}/ssh`；手动触发恢复：节点页离线节点旁的「重连」按钮，即 `POST /api/v1/nodes/{uuid}/recovery/probe`
  （忽略等待窗口，仍跳过在线、未接入和无凭据的节点）。「重连」是这件事在界面上的唯一入口：节点没有 SSH 凭据时，
  按钮不触发 SSH 恢复，改为弹出该节点的 agent 安装命令，到节点上执行即可让 Agent 重新接入；阶段是 `stopped`（云实例已销毁）时按钮置灰，悬停说明原因。
- `--recovery-enabled=false` 时手动触发返回 `recovery is disabled`。参数默认值见[配置参考](../reference/configuration.md)。
- SSH 恢复会 `systemctl restart gpuworks-agent`（无 unit 时 nohup 启动；unit 属于另一个 server 时跳过重启），等 `--recovery-restart-wait`（1m）
  仍不上线才沿用原 token 重装；重装运行的就是安装脚本，按 server 记着的节点信息重写 `<state>/gpuworks-agent.conf`，并沿用主机现在的运行方式（systemd 或 nohup）。
- 登录之前，如果节点关联着云厂商实例，恢复先重读该实例：存着的 SSH 端点已不在实例的入口列表里（平台迁移实例或修改配置后把端口重新编号：RunPod 修改 Pod 配置
  会重启它并重编所有端口，普通的重启不改端口）时，改用实例当前的入口，用户和凭据不变；Server 发起连接的节点，如果平台把 agent 监听端口映射到了新地址（仅 Vast、RunPod 这类按映射
  接入的厂商），就记下新地址并让 server 立刻重拨，这一轮不再登录主机。同一个云账号的实例清单在一轮恢复里最多读一次，30 秒内复用，所以一个账号下有许多离线节点也不会反复调用厂商接口。

## GPU 没被探测到，或厂商工具未响应

**现象**：节点在线但 GPU 数为 0，或节点详情页出现「厂商工具未响应」标记（节点列表不显示）。悬停可看
厂商工具从什么时候起没有返回（连续失败的第一次，探测成功后清除）和具体错误。

> 加速卡探测失败**不会**让节点离线或变成别的状态：节点连着就是「在线」，终端、诊断、重连都照常可用——
> 驱动卡住的节点恰恰最需要登上去看。探测失败时，只有厂商工具才知道的信息（显存、UUID、健康）保留上一次成功探测的结果，
> 并由「厂商工具未响应」标出；哪些卡在机器里、归谁，来自 PCI 总线，不受影响。

**可能原因**

1. 驱动或 `nvidia-smi` 没装 / 不在 `PATH`：prober 返回 0 张 GPU 且不报错，节点在线但没有 GPU。
   agent 按 `hy-smi`、`npu-smi`、`ppu-smi`、`nvidia-smi` 顺序选第一个真正应答的，
   NVIDIA 的判据是 `nvidia-smi --query-gpu=index` 能成功。NVIDIA 排最后，因为 PPU SDK 自带一个
   CUDA 兼容的 `nvidia-smi`（`/usr/local/PPU_SDK/CUDA_SDK/bin`），它会把 PPU 卡当作 GPU 正常应答。
   都不应答时按**厂商设备文件**（`/dev/davinci_manager`、`/dev/alixpu_ctl`、`/dev/nvidiactl`）判，再按**装了哪家 CLI**判
   （`npu-smi`、`hy-smi`、`ppu-smi`、`nvidia-smi` 顺序，NVIDIA 最后：别家主机上常留着一个过期的 `nvidia-smi`）；
   都没有才是「没有加速卡」，厂商为空。驱动还在加载时（昇腾主机开机后 agent 先起、`npu-smi` 尚未应答、
   设备文件也还没建）节点因此仍是昇腾节点，随后的探测能补上卡。
2. 昇腾节点 0 张卡或日志里 `gpu command failed vendor=ascend args=[info] err="npu-smi: no answer within 10s: ..."`：
   `npu-smi info` 超过 10 秒（驱动刚加载时会）。等下一轮探测即可；若持续，在节点上直接跑 `npu-smi info` 看耗时。
3. `nvidia-smi` 存在但执行失败（驱动与内核库不匹配、NVML 报错、超过 10 秒）：agent 日志出现
   `gpu command failed`（带 `output_tail`）和 `gpu probe failed`；server 收到探测失败后保留原有的 GPU 列表、
   把读数标为已过期，dGPU 探针结果也沿用上次的，不因这一轮重跑。节点连接状态不变。
   厂商 CLI 驱动答得慢时（所有调用都在驱动锁上排队，常见于有卡初始化失败的主机），状态上报和指标用的只读查询
   （清单、指标、拓扑、版本）每次最多等 10 秒；等不到时先用同一查询上一次的结果，查询本身在后台继续跑，
   最长 180 秒才 kill，跑完的结果留给下一轮。同一个 CLI 同时只跑一个这样的查询；同一查询的调用方共用一个进程。
   这期间指标标的是那次查询完成的时间，不是当前时间，所以图上会稀疏，但不会出现伪造的新数据点。
   日志 `gpu query slow; callers are served its last answer meanwhile`（带 `took`）说明驱动慢；上一次成功的结果
   超过 10 分钟仍没有新结果时，才按探测失败处理（节点仍然在线，卡的读数标为已过期）。超时只算慢，
   不会换用字段更少的查询。udocker 解析设备节点用的 `nvidia-smi -q -x` 也走这条路。
   NVIDIA 节点上各查询的频率：指标每 10 秒一次；PCIe 吞吐（`nvidia-smi dmon`）每 60 秒一次，其间的指标样本不带
   PCIe 值；清单与外部进程列表随状态上报每 60 秒一次；拓扑（`topo -m`，健康主机上也可能要十几秒）与 MIG 列表
   （`-L`）只在卡变化（序号、UUID、PCIe 地址）时或每小时重查一次；CUDA 版本在驱动版本变化时才重查。
   节点诊断（doctor）同样只用这些共用查询：驱动版本取自清单（`nvidia-smi` 答不上时读内核模块的 `/sys/module/nvidia/version`），不再单独跑 `nvidia-smi`；CUDA 运行时检查没通过时，
   跳过同样要初始化驱动的 peer access 与 NCCL 检查（显示 `SKIP (the CUDA runtime check did not pass)`）。
   server 等 doctor 最多 2 分钟，agent 自己把一次诊断限制在 100 秒内，到点仍在跑的检查被截断，报告照常返回。
   会改驱动状态的命令（如昇腾 `npu-smi set`）和要求即时结果的调用不用旧结果：仍限时 10 秒，超时后被 kill
   仍不退出的（卡在驱动里的 D 状态进程）只等 3 秒就放手，日志 `gpu command did not exit after kill`；
   在它退出（日志 `stuck gpu command exited`）之前，同一个 CLI 的这类调用直接失败（`a previous call has not exited`）。
   持续出现时在节点上 `ps -eo pid,stat,etime,cmd | grep -- -smi` 看有没有 `D` 状态的进程，并查 `dmesg` 里的
   Xid / NVRM 报错。
4. 某张卡健康是「运行时无法使用」（`runtime_unusable`，仅 NVIDIA）：加速卡运行时冒烟每 15 分钟逐卡跑一轮
   （只点名这一张卡，需要 `python3` 和 `libcuda.so.1`；正在跑实例的卡跳过，驱动不应答时整轮跳过），
   在这张卡上创建 CUDA 上下文失败。一张卡初始化失败常常让整机的 `cuInit` 都失败，所以要连别的卡的结论一起看。
   冒烟不改变节点连接状态。
5. 驱动正常但容器里看不到 GPU：缺 NVIDIA Container Toolkit，或（runveil 节点）用户没有设备节点所在的组。
   运行时为自动（未指定 `--driver`、偏好为 `auto`）的 NVIDIA 节点上，Docker / nerdctl 给不了容器加速卡时，agent 跳过它们改用 runveil，
   节点照常调度；节点详情「运行时」一行显示 `auto（当前 runveil）`，并用橙色提示说明被跳过的运行时与原因
   （没装 toolkit 为 `gpu_plugin_missing`，装了但 Docker 没配置为 `gpu_runtime_unconfigured`），附安装或配置命令。
   agent 每分钟复查一次：被跳过的运行时能用了，节点上没有实例时自动切回；有实例时提示「Docker 已可用，可切换」（`gpu_ready`），
   可在「运行时」下拉框手动切换。昇腾节点不做这种回退（ARM 910B 上 runveil 跑不起 CANN）。
   指定了运行时（或昇腾节点）而运行时给不了容器加速卡时（`gpu_plugin_missing` / `gpu_runtime_unconfigured`），
   平台会自动停止该节点的调度：节点列表里「已停止调度」标记悬停可看原因，节点详情的「运行时插件」一行标出缺失的插件并可复制安装命令；
   原因没解决就手动恢复调度时，界面会先警告该原因，下一次上报时平台还会再次停止调度；
   原因消失（插件装好，或 agent 换到不需要插件的运行时）后的第一次上报，平台自动恢复调度
   （平台不记录是谁停的，运维在平台停止之后又手动停止的，也会一并恢复）。
6. 某张卡健康是「驱动初始化失败」（`driver_init_failed`，仅 NVIDIA）：卡在 PCI 上存在、仍绑定 nvidia 驱动，
   但 `nvidia-smi` 列出的卡里没有它，通常是驱动没能把它带起来
   （`dmesg` 里有 `RmInitAdapter failed`、`Possible bad register read`、`GPU may need to be reset`）。
   这类卡仍然出现在加速卡列表里（卡插在机器上是既定事实，agent 直接读 sysfs 枚举 PCI，毫秒级，不依赖驱动），
   只是不可调度，型号取自 `pci.ids`。这类主机上 CUDA 往往整体不可用，部署前先在节点上确认，必要时复位这些卡。
7. 某张卡健康是「读不到信息」（`vendor_cli_unanswered`）：卡绑定着厂商驱动，但本轮厂商 CLI 根本没应答
   （超时，或没装），除 PCI 标识外一无所知——这不等于卡坏了。看节点的 GPU 探测错误，并在节点上手动跑一次 CLI。
8. 加速卡列表里的卡标着「已直通」「未绑定驱动」或「被其它驱动占用」：分别是绑定 `vfio-pci`
   （给虚拟机用）、没绑定任何驱动、绑在 nouveau/pci-stub 这类非厂商驱动上。
   它们都不是故障，也不计入节点的 GPU 数与容量，不参与调度，但会出现在加速卡列表里。卡的列表来自直接读 `/sys/bus/pci/devices`
   （不依赖 `lspci` 命令），四家厂商都一样；厂商 CLI 能列出的卡一定在列表里，PCI 枚举只会补上
   CLI 说不了话的那些（已直通、没驱动、驱动没带起来），不会减少任何一张。
   实测：昇腾主机 `npu-smi` 报 `dcmi module initialize failed` 时，8 张 910B4 仍然全部列出。
   **agent 跑在容器里时不读 PCI 总线**：容器看得到宿主的整条总线，把宿主其它卡当成本节点的会
   凭空多报坏卡，所以容器里只认厂商 CLI 列出的卡。
9. doctor 里出现「有未识别的加速卡设备」（`accelerator_unrecognized`）：本机有属于已支持厂商的
   PCI 设备，但不符合该厂商任何一条识别规则——通常是我们还没见过的新型号。把报告里的 BDF、
   `vendor:device`、class 和绑定驱动发给我们即可补上。这类设备不在加速卡列表里，也不参与调度。
10. 卡标着「被平台外的进程占用」：卡上有不是平台启动的计算进程（比如用户自己起的 vLLM）。
   这张卡在这些进程退出前不参与调度，悬停可看进程名与显存占用。agent 刚重启、还没收到自己
   该跑哪些实例之前不做这个判断——否则自己的实例会被当成外部进程；本轮探测失败时也不做，
   厂商工具刚答不出清单，再问它进程只会再等一个超时。

**怎么确认与处理**

```bash
gpuworks-agent doctor        # 纯文本；退出码恒为 0，问题集中在 "-- action required --" 一节
nvidia-smi
```

- 节点详情的「诊断」按钮跑同一套检查，按界面语言展示：概况、运行时、加速卡（每卡带 BDF 与健康状态）、
  **需要处理**、信息。CLI 输出同样内容的英文版，分节为 `-- runtimes --`、`-- accelerators --`、
  `-- action required --`、`-- facts --`；没有 GPU 时加速卡一节为
  `no accelerator tool found (CPU-only node or driver not installed)`。
- 「需要处理」里每一项都给出**依据**（取证用的命令或调用及其返回值）和**复核**命令，能复现的都可以在节点上自己执行；
  只有厂商工具本身给出恢复方式时才写**处理**命令（如复位）。目前的条目：加速卡健康（见下一节）、
  磁盘可用空间不足 10%（根分区、模型目录、容器数据目录、存储）、已选用的容器运行时无响应、加速卡工具执行失败，
  以及 NVIDIA 的 CUDA 初始化失败和多卡通信检查失败。运行时与文件系统方面另有四项（见[运行时初始化失败与容器节点的文件系统](#运行时初始化失败与容器节点的文件系统)）：
  `runtime_provision_failed`（运行时初始化失败）、`path_noexec`、`path_read_only`、`path_ephemeral`。
- 概况里每个路径（根、模型目录、容器数据目录、udocker 目录、agent 目录、存储）后面带文件系统类型和挂载标记（网络、noexec、只读、容器层）。
- 运行时插件、设备访问（runveil）、设备注入方式、驱动与 Fabric Manager、CUDA / 对等访问 / NCCL 结果、
  udocker 工作负载的运行身份（`runtime_identity`）、网络文件系统（`network_filesystem`）列在「信息」里，只陈述事实。
- 具体探测错误不在节点详情里展示，只在 `agent.log`。
- 厂商、探测命令和运行时矩阵见[支持矩阵](../reference/support-matrix.md)。

## 加速卡「待复位」「显存故障」等健康状态

**现象**：节点详情的加速卡列表里，健康列不是「正常」；鼠标移到 ⓘ 上可看说明、依据与复核命令。

**行为**：agent 每轮探测按厂商工具的字段给每张卡定级，并把取证字段原样存下（字段名与厂商工具一致）：

| 级别 | 调度 | 含义 |
|---|---|---|
| 正常 | 照常 | 无发现，或只有已处理的历史错误（`memory_error_history`） |
| 告警 | 照常，但健康卡不够时才用（评估结果带 `degraded_gpus_used`） | `memory_repair_pending`（待复位）、`vendor_health_warning`（厂商工具 Warning） |
| 故障 | 停止 | `memory_uncorrectable_errors`、`memory_remap_failed`、`reset_required`、`reboot_required`、`power_cycle_required`、`vendor_health_fault`、`runtime_unusable`（运行时用不了这张卡）、`driver_init_failed`（驱动没带起来）、`vendor_cli_unanswered`（厂商 CLI 没应答，读不到信息） |

「能不能用」是一个结论，由 server 在存卡时判定一次（健康等级、卡的归属、平台外占用三者合一），
API 的 `schedulable` 字段就是它，界面与调度都读这一个字段，不各自再推一遍。**不要读 `healthy`**：
直通的卡 `healthy` 为 false 却没有任何故障，被外部进程占用的卡 `healthy` 为 true 却不能调度。
不能用的原因在卡自身的 `health_status`/`health_code`/`ownership`/`foreign_processes` 里。

各厂商的取证字段：

| 厂商 | 命令 | 字段与判定 |
|---|---|---|
| PPU | `ppu-smi --query-ppu=...` | `remapped_rows.pending` / `retired_pages.pending` 为 Yes → 待复位；`ecc.errors.uncorrected.volatile.dram` > 0 → 显存故障；`remapped_rows.failure` 为 Yes → 显存故障；`xid.correctable.ppu_reset` / `os_reboot` / `cold_reboot` 有值 → 需要复位 / 重启主机 / 断电重启；`ecc.errors.uncorrected.aggregate.dram`、`retired_pages.double_bit.count`、`remapped_rows.uncorrectable` 是复位不清零的累计值，只作历史记录 |
| NVIDIA | `nvidia-smi --query-gpu=...` | `ecc.errors.uncorrected.volatile.total` > 0 → 显存故障；`remapped_rows.pending` / `retired_pages.pending` 为 Yes → 待复位；`remapped_rows.failure` 为 Yes → 显存故障；`ecc.errors.uncorrected.aggregate.total` 只作历史记录；卡不支持的字段返回 `[N/A]`，不参与判定 |
| 昇腾 | `npu-smi info`，非 OK 时再 `npu-smi info -t health -i <NPU> -c <Chip>` | Health 为 Warning → 告警；Alarm / Critical → 故障；错误码与错误信息原样展示，含义查华为文档 |

**怎么复核与处理**：说明里的命令都按卡的 PCI 地址生成，可直接在节点上执行。以 PPU 待复位为例：

```bash
ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i 0000:7e:00.0   # 复核：Remapped Rows → Pending
ppu-smi --query-compute-apps=pid,process_name,pci.bus_id --format=csv   # 复位前确认卡上没有进程
ppu-smi -r -i 0000:7e:00.0                                          # 复位；不成功时重启主机
```

复位后下一次节点状态上报（最长约 60 秒）时状态自动更新。显存行替换失败或复位后错误计数继续增长，联系硬件厂商。

## 运行时初始化失败与容器节点的文件系统

**现象**：所有部署都失败，信息形如 `runveil runtime unavailable: <原因>`（例如 `… does not allow ':' in file names …`；较早版本的 agent 写作 `udocker runtime unavailable` 或 `prepare udocker ownership`）；或节点详情「诊断」里出现
`runtime_provision_failed`、`path_noexec`、`path_read_only`、`path_ephemeral`。典型案例是 RunPod 容器节点：agent 装在
`/workspace`（NFSv4 持久卷），root 在其上 `chown` 给其他用户会得到 `Operation not permitted`；而 `/` 是容器自身的 overlay 层，
容器重建后丢失。

**行为**

1. udocker 运行时（runveil）在 root agent 下默认以专用用户 `udocker` 运行工作负载。初始化时 agent 先在 udocker 目录和 state 目录里
   试一次「把新文件交给该用户」；文件系统拒绝（NFS 常见）时改用 agent 自己的用户运行，日志为
   `dedicated runtime user is unusable; running as the agent's own user`，原因记在「信息」的 `runtime_identity` 里
   （例如 `the agent's own user (uid 0): the dedicated runtime user is not used because /workspace/gpuworks/udocker refuses to change file ownership: …`）。
   这不是故障，只是工作负载的身份；它写进模型目录的文件属 root。
2. 初始化还会在 udocker 目录里试建一个文件名带 `:` 的文件。容器镜像离不开这种文件名（镜像层 `sha256:<hex>`、
   Debian 系镜像的 `/var/lib/dpkg/info/<包>:amd64.list`），文件系统拒绝时（RunPod 的 `/workspace`）初始化直接失败，错误写明
   `<目录> does not allow ':' in file names … reinstall the agent with an install directory on a local filesystem`。
   处理：在本地文件系统上重新安装 agent（RunPod 上用容器盘上的路径，并把容器盘开到放得下解包后的镜像）。
   原因详见[为什么 udocker 节点的安装目录不能放在网络卷上](connect-nodes.md#为什么-udockerrunveil节点的安装目录不能放在网络卷上)。
3. 初始化本身失败（目录建不出来、PRoot 释放失败、chown 失败、文件名不允许冒号等）时，错误和时间记下来，`runtime_provision_failed`
   （错误级）带着**依据**（错误全文，chown 失败会含命令的 stderr）、**探测**（初始化做了什么）和**复核**命令
   （`findmnt -T <udocker 目录>; ls -ld <udocker 目录> <state 目录>`）。失败后 30 秒内（再失败则 1、2、4 分钟，上限 5 分钟）
   所有调用方直接拿到记下的错误，不再重复执行初始化；间隔过后下一次调用重试，成功即清除。`doctor` 读状态时不会触发初始化。
4. 拉镜像遇到运行时初始化失败时不再逐个尝试镜像源，直接以 `udocker runtime unavailable: <原因>` 失败，不再写成
   `all image sources failed`——问题在节点，不在镜像源。
5. 路径检查（读 `/proc/self/mountinfo`，推荐以 `findmnt -T <路径>` 复核）：
   - `path_noexec`（错误）：udocker 目录或 agent 目录所在挂载带 `noexec`，运行时无法从那里启动程序。
   - `path_read_only`（错误）：模型目录、udocker 目录或 agent 目录所在挂载只读。存储（storage）和 `/var/lib/docker` 不检查：
     只读的共享模型库是受支持的用法，docker 数据目录归 dockerd 管。
   - `path_ephemeral`（告警）：节点在容器里（存在 `/.dockerenv` 或 `/run/.containerenv`），模型目录、udocker 目录或 agent 目录落在容器自身的
     overlay 层上，容器重建后丢失。实例有本地文件系统的持久卷时把 agent 装到那里；不允许文件名带冒号的网络卷（如 RunPod 的 `/workspace`）放不了 udocker 的镜像（见上文第 2 条），这类实例上只能接受重建后重新拉镜像。
   - 网络文件系统（NFS、CIFS、CephFS 等）不是问题，只作为「信息」里的 `network_filesystem` 列出，并列出它承载的路径。

**怎么确认与处理**

```bash
findmnt -T /workspace/gpuworks/udocker        # 文件系统类型与挂载选项
ls -ld /workspace/gpuworks/udocker /workspace/gpuworks/udocker-state
gpuworks-agent doctor                         # CLI 是独立进程，看不到运行中 agent 的初始化状态与各路径；这些只在节点详情的「诊断」里
```

SSH 安装的节点还会在登录用户的家目录里留一个 `udocker-debug` 脚本，用同样的 `UDOCKER_DIR` 和 PRoot 直接运行 udocker 命令，便于复现。
修复挂载（去掉 `noexec`、改成可写、换到持久卷）后，等记下的错误过期（最长 5 分钟）或重启 agent 即可重新初始化。

## 没有 Docker 的节点（runveil）

**现象**：节点没装 Docker；界面把运行时显示为 runveil（driver 名 `udocker` 是保留名）。

**行为**：`--driver` 缺省时 agent 按 docker → nerdctl → runveil 自动选（macOS 上只有 `host`：引擎是宿主进程，
Docker Desktop 的容器在没有 Metal 的虚拟机里）；NVIDIA 节点上给不了容器加速卡的 docker / nerdctl 会被跳过（见上文第 5 条）；
runveil 只要 Linux 的 amd64 / arm64，
默认启动器不需要 Python。三者都不可用时日志为
`no usable container runtime (docker/nerdctl/udocker); deployments disabled on this node`，节点仍可上线、
上报 GPU，但调度以 `runtime_unavailable` 拒绝，dGPU 原因为 `no usable docker/udocker runtime on the node`。
可用 `--driver` 或 `GPUWORKS_RUNTIME_DRIVER` 强制（`docker|udocker|nerdctl|host|none`；`host` 只在 macOS、`udocker` 只在 Linux 可用，
强制了本机系统跑不了的 driver 或非法值时只在 agent 日志里记原因，节点按无运行时继续运行），
或 `PATCH /api/v1/nodes/{uuid}/runtime` 改偏好（`auto/docker/nerdctl/udocker`）。

**可能原因（runveil 上容器起不来）**

1. 非 root 运行时缺设备权限：`doctor` 显示 `device access: FAIL`。
2. 镜像是旧 udocker CLI 登记的：runveil 不能作为同步源，需重新拉取或导入。
3. 用到 runveil 不支持的能力：RDMA 参数网络（`COMPUTE_NETWORK_RUNTIME`）、PPU dGPU（只在 Docker 上验证）。
4. agent 重启之前启动的工作负载退出时，退出码无从得知，固定报为 1（`… (code 1)`），OOM 也就认不出来；
   当前 agent 进程启动的工作负载如实上报退出码（被信号杀死记为 128+信号，OOM 被杀即 137）。
5. 磁盘不够：解包镜像前可用空间少于「镜像各层大小 + 2 GiB」时直接失败（`not enough disk space to unpack the image …`）；
   兼容性修复时把宿主驱动库拷进容器失败也直接失败（`copy NVIDIA driver library libnvidia-ml.so.1 into the container: … no space left on device`）。
   启动容器进程前，容器目录或日志目录对普通用户可用的空间少于 1 GiB 时也直接失败（`not enough disk space to start the container: …`）：
   agent 以 root 运行，能用文件系统给 root 预留的空间，引擎却以普通用户运行，写不进去。
   较早版本的 agent 会在拷贝失败后照常启动，引擎随即退出（vLLM 报 `Failed to infer device type`），而且磁盘满时连容器输出都写不进去。

容器启动后退出时，失败信息带上引擎最后一行报错，例如 `container exited (code 2): vllm: error: unrecognized arguments: …`；
完全没有输出时写 `… with no output`，这多半是磁盘已满、日志都写不进去。

**怎么确认与处理**

```bash
sudo gpuworks-agent grant-device-access --user udocker --create-user   # 给工作负载用户加设备组
gpuworks-agent runveil ps -a
gpuworks-agent runveil logs -n 200 <容器名前缀>
gpuworks-agent runveil exec <容器名前缀>
```

`runveil` 子命令直接读 state 目录；state 不是默认值（`GPUWORKS_STATE_DIR`，否则 `/opt/gpuworks`）时需同样设置，或在子命令前传 `--state`
（安装脚本把 state 写在 systemd 单元 `ExecStart` 的 `--state` 里，连接配置在该目录的 `gpuworks-agent.conf`）。
详见 [CLI](../reference/cli.md)。

## 模型下载卡住、失败或目录不完整

**现象**：模型在节点上长时间停在 `downloading`，或变 `failed` / `insufficient_disk_space` / `download_required`，
部署创建被拒。

**状态**：`pending`、`downloading`、`syncing`、`present`、`registered`、`failed`、`insufficient_disk_space`、
`download_required`、`absent`。**没有 `partial`**：只有 `present`（下载并校验完整）或 `registered`（本地登记路径）
才算权重完整。节点上不完整时，本地来源的模型创建部署返回 409 `MODEL_SOURCE_UNAVAILABLE`；
公网来源的模型要求模型源 20 秒内应答，否则同样返回该错误，应答则创建成功、启动前再下载。

**可能原因**

1. 节点访问不到 Hugging Face / ModelScope。失败文案前缀：`list files:`、`download <path> from <url>:`；
   列表阶段的 HTTP 错误带 `http <状态码>`，下载阶段带 `unexpected status <状态码>`。
2. 磁盘不足：状态 `insufficient_disk_space`，message 形如 `insufficient disk space at <path>: available=… required=…`
   （要求在下载量之外再留 1 GiB）。
3. 受限（gated）或私有仓库：在设置 → 拉取设置 → [凭据](configure-pull-settings.md#凭据)里为 `huggingface.co` 添加 token（只确认了 Hugging Face，ModelScope 私有仓库尚未验证）。没有凭据时 401 / 403 会让该地址冷却并换下一个候选，全部候选失败后 `failed`。
4. 限流或服务端错误：HTTP 状态码错误（含 404、429）不在同一地址上重试，该地址冷却 30 分钟并换下一个候选，最后一个地址上才算失败；
   连接重置、超时、EOF 等传输层错误在后面还有候选的地址上最多重试 6 次，最后一个地址保留 16 次（退避 1 秒起、上限 15 秒）。
   所有候选是站点规则的替代地址加站点本身，进度和失败文案里的 host 说明试过哪个、为什么切换，见[配置镜像与模型拉取](configure-pull-settings.md#节点怎样依次尝试)。
5. 源站不支持 Range：续传会从头开始。单个 HTTP 请求（含读完响应体）最长 15 分钟，到时按传输层超时续传重试；
   连续 5 分钟收不到响应头或任何字节的请求会更早被判为卡死并同样续传重试。每次切断都计入该地址的重试次数（最后一个地址每文件 16 次），
   所以单个文件在慢链路上累计约 4 小时仍未下完时会失败；源站不支持 Range 时每次重试都从头开始，大文件在慢链路上可能始终下不完。
6. 显式选的源节点离线或副本不完整：停在 `download_required`，不会静默换源。
7. 自动复制的源节点（同步组内持有模型的节点）连不上（例如两台节点的管理网地址在纸面上同网段、实际互不可达）：目标节点自动改为下载，这一对节点 3 小时内不再尝试复制；没有公网来源时停在 `download_required`。
   模型卡在某台节点上（`failed` 或 `download_required`）时，部署的等待原因是 `model_unavailable`，写明模型、节点和错误；自动选节点的副本会跳过这台节点改放别处。

**怎么确认与处理**

- 看该模型在节点上的状态与 message，以及日志页 `source=agent` 同一时间段的输出。
- 半成品语义：写入中的文件是 `<文件>.part` 与 `<文件>.part.state`，重试会续传；完整性以目录内的
  `.gpuworks-model-complete.json` 为准，文件清单和大小全部吻合才报 `present`。手工登记路径得到的是 `registered`，不会变成 `present`。
- 重试公网下载：`POST /api/v1/models/{id}/download`，body `{"node_uuid":"<uuid>"}`（状态重置为 `pending`）。
  失败的部署不会自动重发下载。没有公网下载的取消接口；需要清理时 `DELETE /api/v1/models/{id}/nodes/{uuid}`
  （有活动部署使用时 409 `MODEL_IN_USE`）。
- 国内网络：在设置 → 拉取设置里配置代理或站点规则（例如把 `https://huggingface.co` 指向镜像站或内网代理），示例见[常见网络配置示例](configure-pull-settings.md#常见网络配置示例)；可以用站点规则抽屉的「测试连通」先验证。
- 部署提示 `weights_fetch_forbidden` / 下载接口 409 `WEIGHTS_FETCH_FORBIDDEN`：适用于这台节点的规则把所有候选都禁止了（关闭回源且没有替代地址）。改规则，或在有模型的节点上「登记路径」、把模型放到共享存储。
- 登记模型后没有元数据、响应带 `metadata_notice`（`MODEL_METADATA_UNREACHABLE` / `MODEL_METADATA_FORBIDDEN`）：管理节点连不上站点；给能用的站点规则勾选「用于管理节点获取模型元数据」，必要时给代理勾选「用于管理节点出站访问」。
- 公网不通但别的节点已有副本：走节点间同步；没有共同网络时报 `NO_STORAGE_PATH`。见
  [下载、缓存和节点间同步模型](download-and-sync-models.md)、[从 HTTP 导入](import-from-http.md)。

## 镜像拉取失败

**现象**：实例停在 `pulling`，事件里出现 `image source failed: <ref>: …`，最终 `all image sources failed: …`；
或镜像页该节点状态为 `failed`（镜像状态：`absent`、`pulling`、`present`、`syncing`、`failed`）。

**拉取候选顺序**：部署不选择来源，每次拉取（启动、重放、扩副本、多机预拉取、镜像页「拉取到节点」）用同一份候选：
适用于该节点的[来源规则](configure-pull-settings.md#来源规则)产生的替代地址（规则顺序）→ 镜像自己的副本：模板声明的
`image_sources`（仅限部署用的就是模板自己的镜像，见[模板语法](../reference/inference-catalog-spec.md#image_sources模板声明的镜像来源)）→
目录登记的 `registry_sources`（官方镜像在 Docker Hub、quay.io、华为云 SWR 各有一份）→ 规范（canonical）地址。镜像自己的副本由节点探测
`https://<host>/v2/`（5 秒超时，缓存 30 分钟）按可达性和延迟排序；前一个失败才试下一个。下载阶段 3 分钟没有新增字节视为卡住，换下一个候选；
失败或卡住的仓库在这台节点上冷却 30 分钟，排到最后但不剔除。nerdctl 节点上超过 1 GiB 的层，进度只按 0.1 GiB 变化，卡住时限相应放宽到约 17.5 分钟。消息里出现 `Progress output not understood, stall check off` 表示 agent 读不懂这个运行时版本的进度输出，这次拉取不判卡住（agent 日志有原始行）。解包阶段（「解包镜像层」）不判卡住，每个候选从开始拉取到解包完最多 60 分钟；拉取 docker 时分配不到伪终端就没有字节进度，卡住判定对它不生效，只受这个上限约束。
配置方式见[配置镜像拉取](configure-pull-settings.md)。

某台节点的规则让一个镜像什么候选都不剩（没有替代地址又关闭了回源）时，调度原因为 `image_source_forbidden`（`node`、`image`），副本不会在该节点启动；
到设置 → 拉取设置 → 来源规则调整。

模板里 digest 钉死的镜像（`repo@sha256:…`）由服务端给出的任一候选满足（模板声明的 `image_sources`、来源规则的替代地址）：
节点拉到哪个候选就直接用它启动容器，不再打回规范引用（运行时不能给 digest 引用打 tag），候选已在本地时同样直接使用；
节点不核对候选的 digest 是否与模板镜像一致，候选归模板作者和部署者负责。失败信息里按引用列出每个候选的原因。

**可能原因**

1. 节点到 registry 不通：代理、DNS、防火墙。docker 节点的代理要写进 dockerd，改了没生效见[代理与 dockerd](configure-pull-settings.md#dockerd)。
2. 替代地址（来源规则里的镜像站、Harbor）不可用或没有该 tag：候选依次失败，看 `image_pull_retry` 事件里每个来源的错误。
3. 私有仓库需要登录：在[凭据](configure-pull-settings.md#凭据)里配置；tag 写错。digest 引用不被支持（`digest image refs are not supported`）。
4. 节点磁盘不足（调度阶段为 `insufficient_runtime_disk`；归档导入时报 `insufficient disk space at …`）。
5. 归档下载导入、节点间传输（镜像状态 `syncing`）15 分钟没有进度上报：被置 `failed`，提示 `this transfer stopped reporting progress`，重试即可
   （节点上其实已有该镜像时改为 `present`；registry 拉取不受此超时约束）。

**怎么确认与处理**

- 实例事件时间线逐条列出每个来源的失败原因；UI 对该阶段的标签是「镜像来源失败」。
- 重试：`POST /api/v1/images/{id}/nodes/{uuid}/sync/retry`，同样按拉取设置重新计算候选，不能指定仓库（指定会报 `IMAGE_SOURCE_REMOVED`；适用于该节点的规则既没有替代地址又关闭了回源时报 `IMAGE_SOURCE_FORBIDDEN`，部署副本则以 `image_source_forbidden` 拒绝启动）；换镜像站在设置 → 拉取设置 → 来源规则里改（规则不合法报 `PULL_SETTING_INVALID`）。
- 无公网：来源规则指向内网镜像仓 / 归档 URL / 节点间同步，见[离线环境部署](deploy-offline.md)、[从 HTTP 导入](import-from-http.md)。
- 滚动 tag（`latest`、`nightly` 等）在节点上已是旧内容：镜像详情的节点行点「重新拉取」
  （`POST /api/v1/images/{id}/nodes/{uuid}/sync` 带 `"force":true`），完成后镜像 ID 列显示新 digest。

## 部署被拒、显存估算不足或无合适节点

**前提**：`POST /api/v1/deployments` 创建时不按当前占用做显存调度，成功返回 201（例外：单机创建会按空载试算候选池，
没有一种硬件放得下就返回 400 `DEPLOYMENT_NEVER_FITS`；`placement_mode: "auto"` 的单机创建还会把评估时的放置方案重跑一遍，
不再容纳就返回 409 `AUTOMATIC_PLACEMENT_STALE`）；显存和放置结论出现在
`POST /api/v1/evaluate` 的响应里，以及创建后部署事件的 `DEPLOYMENT_UNSCHEDULABLE`（`params.scheduler` 是调度器给出的原因）。

| 错误码 | HTTP | 含义与处理 |
|---|---|---|
| `AUTOMATIC_PLACEMENT_STALE` | 409 | 评估之后资源被占走或节点不再满足节点条件；重新评估 |
| `NODE_REQUIREMENTS_UNMET` | 400 | 所选节点不满足部署的节点条件；`params.detail` 写明哪个键、要求哪些值 |
| `NODE_SCHEDULABLE` | 409 | 强制驱逐只用于已停止调度或驱逐中的节点；先停止调度或驱逐，否则撤下的副本会被放回原处 |
| `DEPLOYMENT_NEVER_FITS` | 400 | 创建或编辑节点条件时，候选池里没有一台节点在空载时放得下一个副本（卡太少、单卡显存不够、精度不支持、不能承担服务流量）；`params` 给出候选节点数、硬件规格数与第一台的原因。调小配置（TP、量化、上下文长度）或放宽节点条件 |
| `NODE_REQUIREMENTS_INVALID` | 400 | 节点条件本身无效（拼错的系统标签键、两个厂商、与 `target_vendor` 冲突、多机/PD 部署写了 `gpuworks.ai/node`） |
| `LAUNCH_CONFIG_CONFLICT` | 400 | 表单字段与自定义参数里同一个键取值不同；只保留一处（参数为 `key`、`field_value`、`flag`、`extra_value`） |
| `ACQUISITION_CANDIDATE_STALE` | 400 / 409 | 请求指定的保存存储已不存在或不再存有模型，或所选权重来源已不在模板里；重新评估，服务端不会替换 |
| `MODEL_SOURCE_UNAVAILABLE` | 409 | 权重在所选节点不完整，或模型源 20 秒无应答 |
| `DISTRIBUTED_*`、`REPLICAS_*`、`MASTER_PORT_INVALID` | 400 | 多机 / 副本参数不合法 |

**显存不足**（`schedule` 原因码，见评估响应与 `schedule.explain`）：

| 原因码 | 含义 |
|---|---|
| `no_fit` | 每卡启动固定需求 > 可用显存；参数含 `required_gib`、`available_gib`、`tp`、`gpu_memory_utilization` |
| `insufficient_empty_gpus` / `insufficient_shared_gpus` | 空闲整卡 / 可共享的卡不够 |
| `no_free_gpu` | 没有空闲的 GPU |
| `no_healthy_gpu` | 节点自己的卡都不在能服务的健康级别 |
| `no_owned_gpu` | 节点没有归本机使用的卡：一张都没有，或全部直通给虚拟机、未绑定驱动、被其他驱动持有 |
| `gpus_held_outside_platform` | 卡本身健康，但能用的都被平台外的进程占着 |
| `tp_head_count_not_divisible` | TP 不能整除注意力头数 |
| `sleep_residue_blocks_startup` | 休眠实例的残留显存挡住了启动 |
| `engine_ports_exhausted` | 节点引擎端口区间用尽 |
| `insufficient_disk` / `insufficient_runtime_disk` | 权重盘 / 运行时盘不足 |
| `runtime_unavailable` / `serving_unavailable` | 节点没有容器运行时 / 没有可承担服务流量的网络 |
| `image_source_forbidden` | 节点适用的拉取来源规则让镜像无处可拉（参数 `node`、`image`），见[镜像拉取失败](#镜像拉取失败) |
| `partition_unavailable` | 需要 dGPU 后端但节点没有就绪的，见 dGPU 一节 |

启动固定需求 = 权重 / TP + 多模态编码器 / TP + 预留，**不含 KV**；容量 = 显存 × `gpu_memory_utilization`
（dGPU 时改为：需求取显存上限加预留，容量取卡上剩余可用显存）。所以 `no_fit` 时：增大 TP、提高 `gpu_memory_utilization`、选显存更大的节点，或停掉 / 休眠
占卡的其他部署（评估会给出「休眠某某可释放约 N GiB」的建议）。降低 `max_model_len` 只影响 KV 估算，
**不能**解决 `no_fit`。

**怎么确认**：先在部署页看评估结果（显存拆分、候选原因），再用 `GET /api/v1/deployments/{id}/events` 看创建后的调度事件。
估算原理见[调度与显存估算](../explanation/scheduling-and-estimation.md)，选卡与标签见[部署模型](deploy-models.md)。

## 实例启动失败、反复重启或 OOM

**现象**：实例 `failed`；部署反复新建实例又失败；容器被杀。

**重试规则**：平台不靠 Docker 的重启策略（容器以 `--restart no` 启动），由 controller 重建：每个 unit
最多 3 次尝试，新尝试距上一次尝试创建至少 15 秒，重建用新的 UUID、旧失败记录保留作证据。预算用尽后写一条
`UNIT_RETRY_EXHAUSTED` 事件（`params` 含 `attempts`、`reason`），不再自动重建。

**启动健康检查预算**：实例启动后，agent 在这段时间内等引擎的健康检查通过，超时即判这次启动失败（`health check
timed out`）、按上面的规则重建。集群默认 1800 秒，在设置页「部署」卡片修改；单个部署在部署表单「运行环境」的
「启动超时」里另设（`start_timeout_sec`，60–86400 秒），留空跟随集群默认。值在实例创建时定下，修改只影响之后
启动的实例。大模型首次启动要编译内核、加载上百 GB 权重，日志一直在推进却被判超时时，调大这个值，而不是排查
引擎本身。

**可能原因**

1. 显存不够：引擎报 `CUDA out of memory`；容器被 OOM 杀时 agent 报 `container OOM-killed`
   （判据：运行时 OOM 标志，或退出码 137 且最近日志含 `OutOfMemoryError` / `CUDA out of memory`）。
   nerdctl 不提供 OOM 标志，只能靠后一条判据。
2. 镜像或命令不对：`container exited (code N)`、`run failed: …`、`pre-start preparation failed: …`；
   glibc 太旧时 `container glibc too old`。
3. 容器运行时创建容器太慢：`run failed: docker run timed out after 2m0s`（nerdctl 为 `nerdctl run …`）。
   `run -d` 超过 120 秒未返回即判这次启动失败，与上面的启动健康检查预算无关。常见于节点根盘被其他负载写满
   （dockerd 创建容器时卸载临时 overlay 要等根文件系统回写），可在节点上 `time docker create <镜像> true`
   复现。agent 不会杀掉超时的 `run`，等它返回后删掉它建出的容器（最多等 10 分钟，之后杀掉并按容器名删除），
   所以不会留下无人认领的 `gpuworks-*` 容器。
4. 启动太慢：`health check timed out`（多机 follower 为 `headless worker readiness timed out`），
   大模型加载或 CUDA graph 捕获可能需要更长时间。
5. 多机任一 rank 失败：整代失败，消息为 `rank N on <node> failed: …`。
6. 进入 running 之后容器退出（节点上被 `docker stop`、进程崩溃、OOM，或引擎以 0 自行退出）：agent 每 15 秒检查一次
   running 实例的容器，退出即先回收容器、再上报一次 `failed`，消息为 `container exited while running (code N)`
   （OOM 为 `container OOM-killed while running (code 137)`，多机 follower 为 `headless worker …`，
   容器被节点上的人删掉为 `container disappeared while running (removed outside the agent)`），随后按上面的重试规则重建。
   休眠中的实例容器仍在运行，不会被误报；检查容器失败（如 `docker inspect` 出错）只跳过这一轮。
   回收失败时实例改报 `removing`，消息带 `cleanup pending: …`。

**怎么确认与处理**

```bash
curl -s -H "Authorization: Bearer <jwt>" \
  "http://<server>:8800/api/v1/instances/<uuid>/events?limit=20"   # 失败原因在 params.detail
```

- 引擎输出：实例详情的实时日志（`GET /api/v1/instances/{uuid}/logs`，SSE），历史在日志页，
  用 `CUDA out of memory`、`Error` 检索；容器已被清理时用节点上的 `docker logs` 或 `gpuworks-agent runveil logs`。
- CUDA OOM 常见处理：确认卡上没有其他进程占显存（`nvidia-smi`）、降低 `gpu_memory_utilization` 或 `max_model_len`、
  增大 TP。修改后重新部署。
- 预算用尽后修好问题，再调 `POST /api/v1/deployments/{id}/units/retry`，body `{"role":"","index":0}`
  （普通部署 `role` 为空）。返回 404 `UNIT_UNKNOWN`、409 `UNIT_NOT_FAILED`（没有失败尝试或仍有实例）、
  409 `UNIT_RETRY_NOT_ACTIVE`（部署不是运行中）。见 [ADR 0029](../adr/0029-deployments-reconcile-as-units.md)。

## 网关返回 401、404、503

网关错误体形态与管理 API 不同：`{"error":{"message":"…","type":"gpuworks_proxy"}}`，没有 `code`，按 `message` 判断。

| 状态 | message | 原因与处理 |
|---|---|---|
| 401 | `invalid or missing API key` | 缺 `Authorization: Bearer <key>` 或 key 已删。库里**一个 key 都没有**时网关不鉴权 |
| 400 | `request body must specify a model` | 请求体没有 `model` |
| 404 | `no deployment serves model <name>` | 模型名不是部署的 served name（`GET /v1/models` 可列出），或部署已停止 |
| 404 | `deployment <name> does not expose <METHOD> <path>` | 只有试验场（`/api/v1/playground/v1/*`）会做 contract 检查并返回它：该部署的 API contract 不包含这个接口（比如对 Chat 模型调 `/v1/embeddings`）；`GET /api/v1/playground/openapi.json` 可查看它声明了哪些接口。公网 `/v1` 不做这项检查，请求原样转给引擎，由引擎自己返回 404 等 |
| 404 | （`/v1` 整个路径 404） | 访问的监听地址不承载推理 API，检查 `listen.gateway` |
| 503 | `model is asleep; wake it first` | 实例已休眠，调 `POST /api/v1/deployments/{id}/wake`；休眠指令一发出就不再转发 |
| 503 | `no running instance for <name> (still starting?)` | 没有 running 副本：还在启动、失败、被停止或节点离线；看实例状态 |
| 503 | `distributed model <name> is not ready: rank N is unavailable`、`distributed model leader is offline for <name>`、`distributed model leader is not ready for <name>`、`distributed deployment node set is incomplete for <name>` | 多机部署的 rank 或 leader 没就绪 |
| 503 + `Retry-After: 2` | `upstream error: … data lane unavailable: … did not reattach within 5s` | 节点数据通道断开后 5 秒内没重连；**可重试**，不会回落到控制通道 |
| 503 + `Retry-After: 2` | `upstream error: … data lane unavailable: … data lane closed under an open stream: …` | 请求已经发到节点、还没收到响应时数据通道断开；**可重试**。响应开始返回之后才断开的，客户端看到的是被截断的响应 |
| 503 + `Retry-After: 2` | `no prefill instance could take the request (…)` | PD 部署没有可用的 prefill 实例 |
| 502 | `upstream error: …` | 到引擎的隧道或连接失败，查节点在线性和引擎日志 |
| 413 | `request body exceeds N bytes` | 超过 `--proxy-max-body-bytes`（默认 256 MiB） |

只有数据通道不可用和 PD 无 prefill 两类 503 带 `Retry-After`；休眠和启动中的 503 需要先处理实例，而不是重试。
数据通道机制见 [ADR 0024](../adr/0024-data-lane-no-ctrl-fallback.md)。其余差异见[OpenAI 兼容网关](../reference/openai-gateway.md)。

## dGPU 能力 unavailable

**现象**：部署带显存上限时评估出现 `partition_unavailable`，或创建返回 400 `DISTRIBUTED_PARTITION_BACKEND_UNAVAILABLE`
（文案 `partition backend <id> is unavailable: <reason>`）。

**读取状态**：`GET /api/v1/nodes` / `GET /api/v1/gpus` 响应里的 `partition_capabilities[]`，每项的 `status` 只有
`ready` / `unavailable`，`reason` 是唯一原因来源；非 NVIDIA 后端另有 `probe_status`（`passed` / `skipped` / `failed`）。

**可能原因（`reason` 文案，按判断顺序）**

1. `container inference unverified: no usable docker/udocker runtime on the node`：节点没有可用运行时。
2. `container inference unverified: <原因>`：后端对运行时有约束（如 PPU 只在 Docker 上验证）。
3. `agent has not reported current dGPU safety capability; upgrade the agent`：agent 太旧。
4. `missing dGPU assets: <文件列表>`：dGPU 资产缺失。
5. `dGPU probe has not completed: …`：探针还没出结果，稍等或看 agent 日志。
6. `dGPU probe failed: <detail>`：探针失败；探针 `skipped` 时回到 `container inference unverified: <探针 detail>`（如 `no container runtime available on the node`）。detail 来自探针容器，如
   `the container runtime cannot load the embedded probe image`、`dGPU probe timed out`（15 秒）、
   `probe container exited with code N`。

另外两类不是能力问题：未指定 `partition_backend` 时默认按 `nvidia-dgpu` 处理，非 NVIDIA 节点必须显式指定；
给了后端却没给 `vram_limit_gib` 时报 `partition_backend requires a positive vram_limit_gib`。

**怎么确认与处理**：`nvidia-dgpu` 不需要探针，只看运行时与资产；`ascend-dgpu` / `ppu-dgpu` 要探针通过。
探针每个 agent 进程默认跑一次并缓存（资产、GPU 或运行时变化时会重跑），修好环境后重启 agent 重新探测。不要退回整卡假装成功。
细节见[启用和排查 dGPU](dgpu.md)、[支持矩阵](../reference/support-matrix.md)。

## sleep / wake 失败

**现象**：点「休眠」「唤醒」报错，或实例在休眠 / 唤醒后变 `failed`。

| 错误码 | HTTP | 含义 |
|---|---|---|
| `SLEEP_NOT_ENABLED` | 400 | 部署创建时没开可休眠 |
| `SLEEP_ENGINE_UNSUPPORTED` | 400 | 引擎不支持（只有 vLLM 支持） |
| `PD_SLEEP_RDMA_UNSUPPORTED` | 400 | 可休眠的 PD 部署不能让 KV 走主机 RDMA 网络：关闭可休眠，或让节点改用以太网参数网络 |
| `SLEEP_INVALID_LEVEL` / `SLEEP_INVALID_MODE` | 400 | `level` 只能 `1` 或 `2`，`mode` 只能 `wait` 或 `abort` |
| `WAKE_INSUFFICIENT_GPU_MEMORY` | 409 | 卡已被占用，放不回休眠实例；`params` 含 `gpu_indexes`、`shortfall_gib`、`occupants`，先停止或休眠占用者 |

**可能原因（agent 侧失败）**

1. 引擎没有 sleep 端点（自定义镜像或没开 `VLLM_SERVER_DEV_MODE=1`）：事件里是
   `sleep level N → M failed: <url>: http 404`。
2. L2 唤醒失败，或失败后无法确认引擎真实状态（请求超时、状态探测 / 恢复失败）：状态不确定，实例被标 `failed`（phase `sleep`），消息
   `sleep level uncertain after …`，保留完整显存占用并禁止网关转发，controller 会重建这一代实例。
3. 可确认的失败：期望回退（唤醒失败回到休眠，休眠失败回到醒着），事件 `INSTANCE_SLEEP_RESULT`，
   `params.result` 为 `failed_rollback`（成功为 `applied`）。失败不会自动重试。

**怎么确认与处理**：看实例时间线里 `params.phase` 为 `sleep` 的条目及 `params.detail`；修复后再点一次按钮。
见 [sleep / wake](sleep-wake.md)。

## 多机、NCCL 或 RDMA 网络不通

**现象**：多机部署卡在启动，rank 之间握手或 NCCL 初始化超时；`DISTRIBUTED_NODE_OFFLINE` / `COMPUTE_NETWORK_*` 被拒。

**可能原因**

1. 节点之间的路由、防火墙或端口不通（master 端口须在 1024–65535）。
2. 各节点的参数网络不一致：`COMPUTE_NETWORK_MIXED`（有的节点有参数网络、有的没有，且管理网不能承载）、
   `COMPUTE_NETWORK_MISMATCH`（不在同一参数网络，且管理网不能承载）、`COMPUTE_NETWORK_NONE`（没有能承载参数流量的网络），
   以及 runveil 节点不能用 RDMA 的 `COMPUTE_NETWORK_RUNTIME`。管理网允许承载参数流量时，部分 rank 有参数网络会整组回落到管理网并给出
   `COMPUTE_NETWORK_FALLBACK` 警告。
3. RDMA 是强制 IB（平台注入 `NCCL_IB_DISABLE=0`、`NCCL_NET=IB`、`NCCL_IB_HCA`）：容器看不到 verbs 设备时启动即失败，
   **不会**悄悄退回 TCP。
4. HCA 选错或链路有错误计数：看 `gpuworks_node_rdma_events_per_sec` 的 `link_downed`、`symbol_error` 等。
5. 节点列表不合法或有节点离线：`DISTRIBUTED_NODE_COUNT`（`node_uuids` 数量须等于 `nnodes`）、`DISTRIBUTED_DUPLICATE_NODE`、`DISTRIBUTED_NODE_OFFLINE`。

**怎么确认与处理**

- 创建前先跑 `POST /api/v1/deployments/preflight`（2–16 个节点）：单个节点对失败不让请求失败，而是返回 200、
  `ok:false`，`pairs[].error` 写原因，`warnings[]` 里是 `src -> dst: <detail>`。
- 网络详情页的「内部测试」标签页在宿主机上跑 TCP / `ib_write_bw` 互测，「监控」标签页看网卡与 RDMA 速率和错误计数，
  见[配置节点网络](configure-networks.md#内部测试)与[网络指标参考](../reference/network-metrics.md)。
  内部测试不覆盖容器内 rank 之间的 NCCL 路径。
- 日志页检索 `NCCL`，并对照平台注入的 `NCCL_SOCKET_IFNAME`、`GLOO_SOCKET_IFNAME`（实例详情可看保存的 env）。
- 多机流程见[多机推理](multi-node-inference.md)。

## 反代后流式输出被攒批，或长请求被截断

**现象**：`stream: true` 的回复一次性整段返回（首 token 延迟等于整段生成时间）；长生成在固定时长后断开；
大请求体被拒。

**可能原因**

1. 反代开了响应缓冲（Nginx 默认 `proxy_buffering on`），SSE 被攒批。
2. 反代的读超时（Nginx `proxy_read_timeout`）小于非流式长生成的耗时。server 自身对 HTTP 连接不设读写超时，
   长流只会被中间的反代、负载均衡或客户端 SDK 的超时切断。
3. 反代的请求体上限过小（Nginx 默认 `client_max_body_size` 为 1m）。区分方法：来自反代的是 HTML 错误页，
   来自 server 的是 JSON 的 `request body exceeds N bytes`。
4. 用了 `cloudflare-quick` 的临时公网地址：它不支持 SSE，见[给控制台一个公网地址](expose-public-address.md)。
5. server 重启、升级或节点 agent 断线会断开在途的流，且不会重试。

**怎么确认与处理**：直连 server 端口对比同一请求；`/v1/` 的 Nginx 配置要点（`proxy_buffering off`、
`proxy_request_buffering off`、`client_max_body_size`、`proxy_read_timeout`）见[生产环境部署](deploy-production.md)。
长生成一律用 `stream: true`。

## 忘记 admin 密码

**现象**：无法登录。

**处理**：在 server 机器上，以操作员身份（不需要 sudo，也不必停 server）：

```bash
gpuworks reset-password --password '<新密码>' --data /opt/gpuworks/server/data
```

`--user` 缺省为 `admin`，`--data` 缺省为 `./data`（数据库路径可用 `--db` 直接指定）；`--password` 不能为空；
用户不存在时报 `not found`。命令只更新数据库里的密码哈希，已经签发的登录令牌到期前仍然有效；网关 API key 不受影响。
见 [CLI](../reference/cli.md)。

## 仍未解决？

提 issue 或求助时附上这些信息，能省掉大部分往返：

1. 版本：`gpuworks version`（server）和 `gpuworks-agent version`（出问题的节点）。
2. 出问题的实例 UUID 及其事件时间线：`GET /api/v1/instances/<uuid>/events`（必要时加
   `GET /api/v1/deployments/<id>/events`），以及实例响应里的 `schedule` 字段。
3. 日志，限定在故障时间段前后几分钟：server 的 `gpuworks-server.log`，节点的 `agent.log`，
   日志页里该实例的引擎输出。
4. 节点上 `gpuworks-agent doctor` 的完整输出。
5. 连接类问题附 `GET /api/v1/agent/rejects` 的结果和 `gpuworks-agent smoke` 的退出码，以及部署请求体与网络拓扑（是否反代 / NAT）。
   分享前删除 API key、节点 token 和其他凭据。
