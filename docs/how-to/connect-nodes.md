# 接入、升级和卸载节点

**适用场景**：把一台 GPU 主机接入已运行的 server，或升级、卸载已接入节点上的 agent。

**前置条件**：

- server 已经启动（见[安装和升级控制面](install-server.md)或[从零启动并部署第一个模型](../tutorials/getting-started.md)）。Agent 发起连接（默认）时还要知道节点能访问到的 server 地址（安装命令的 `--server`）；Server 发起连接的节点不需要这个地址。
- 已在节点页创建节点（状态为「连接中」），取得专属 `--token`；目标主机有 Docker，没有 Docker 或没有 root 时 agent 使用内置 runveil。
- 只有需要 `server_dial` 时才要求 server 能 SSH 到该主机并访问其监听端口，默认的 `agent_dial` 不需要节点入站端口。

## 选择接入方式

| 方式 | 命令/入口 | 适用 |
|---|---|---|
| curl | `/install.sh` + `--token` | 默认、NAT、容器 |
| SSH 向导 | 节点页“SSH 安装” | server 可 SSH 到主机，可自动预检查 |
| 离线包 | `sh gpuworks-agent.run --token …` | Server 发起连接、没有 SSH、节点下载不到 server 的主机，见[离线包](#离线包) |
| 云主机一键安装 | “公有云”页 | RunPod、优云智算（CompShare）、AutoDL、Vast.ai；步骤见[租用云 GPU](rent-cloud-gpus.md) |

安装脚本支持 `--token`、`--server`、`--connect-mode`、`--listen`、`--driver`、`--mount-root`、`--udocker-bundle`、
`--state`、`--install-dir`、`--supervisor systemd|nohup|keep`（`--no-systemd` 即 `nohup`）、`--no-sudo`、`--clean-existing`，
逐项说明见 [CLI 与安装脚本](../reference/cli.md#节点安装脚本-installsh)。token 必须显式提供，脚本不会内置集群级秘密；`--server` 只有
Agent 发起连接时才必填，Server 发起连接的节点不涉及 server 地址。

安装把节点的启动配置（server、token、连接方向、监听地址、driver 等）写进 state 目录的 `gpuworks-agent.conf`（权限 0600，见
[安装布局](../reference/install-layout.md#agent-配置文件)），systemd 单元、nohup、launchd 都只运行 `gpuworks-agent run --state <目录>`，
token 不会出现在 `ps`、单元文件或 plist 里。要改配置就重新执行安装。主机上已有的 agent 属于另一个 server 时，脚本在 stderr 打一行
`WARNING` 再替换（读的是单元所指目录或本次 state 目录里的配置文件）。

SSH 向导的「SSH 目标」填 `user@host[:port]`，也可以直接粘贴云厂商给的 ssh 命令，例如
`ssh -p 51223 root@203.0.113.7 -L 8080:localhost:8080`：server 只取其中的用户（`-l` / `-o User`）、
主机和端口（`-p` / `-o Port`），端口转发、`-i` 等其余选项和远端命令都忽略；跳板机（`-J`、`ProxyJump`）
不支持。安装目录与「通过 systemd 启动」在「高级」里；Server 发起连接时，「Agent 监听地址」与「计算节点接入地址」在表单主体里。

「通过 systemd 启动」默认开启：Linux 主机有 systemd 且安装用户是 root 或有免密 sudo 时，agent 注册为
`gpuworks-agent.service`，开机自启、崩溃后自动拉起。关闭后（SSH 向导传 `no_systemd`，手工命令带 `--no-systemd`）
agent 以 nohup 后台运行，安装目录里的 `agent-restart.sh` 用于手动重启，开机不自启；主机上已有的 `gpuworks-agent`
服务和其他目录里的 agent 都不动，所以换一个安装目录就能在同一台主机上再加一个节点的 agent。SSH 向导此时不再因为
「主机已是节点」或已有服务而要求确认，只在同一安装目录里已有 agent 在运行时询问（继续会替换它）。没有 systemd 或没有
权限的主机本来就走 nohup；Mac 始终用 launchd。之后的 SSH 带外重启与重装沿用主机当前的方式：unit 里是本节点的 token
就用 systemd，否则（只有 `agent-restart.sh`，或 unit 属于别的节点）走 nohup，不碰别的节点的 unit。SSH 安装（向导与云主机
一键安装，云主机经云助手等命令通道安装时也一样）会记下实际用的安装目录（含回退到的 `/tmp/gpuworks`），带外重启与重装都在这个目录里进行。
只有安装目录留空（默认 `/opt/gpuworks`）且写不进时才回退到 `/tmp/gpuworks`；填了的目录写不进时安装失败，不会换地方。用安装脚本手工接入的
节点没有这条记录，带外恢复按默认的 `/opt/gpuworks` 处理，所以同一台主机上的第二个 agent 建议用 SSH 向导安装。

认证方式默认是**平台密钥**：server 自己持有的一对 SSH 密钥（设置页「SSH 密钥」可查看公钥、导入已有私钥或重新生成，
见 [ADR 0035](../adr/0035-platform-ssh-key.md)）。表单给出一行授权命令，在目标机上以 SSH 用户身份执行一次即可：

```sh
mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '<平台公钥>' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys
```

也可以选「密码」或「私钥」：登录成功后 server 会把平台公钥追加到目标用户的 `authorized_keys`（已有则跳过），
之后只保留平台密钥，不保存你填的密码或私钥；写入失败时才改为保存所填凭据。安装结果里固定有两行：
「SSH authentication」写明试了哪些凭据、哪个被接受，「Platform key」写明公钥已在、本次写入或写入失败的原因。
预检查不写 `authorized_keys`，也不创建任何目录（唯一的写入是安装目录检查用 `mktemp -d` 建的私有临时目录，探测结束即删除，见下）。节点详情「SSH 带外通道」同样三种方式，保存密码时也会顺手写入平台公钥。

## Mac（Apple silicon）节点

M1–M5 的 Mac（macOS 15 及以上）用同一条 `/install.sh` 命令接入，server 可以就跑在这台 Mac 上（server 的
darwin 构建），也可以在别处。Intel Mac 不支持。

- 安装脚本识别 macOS 后下载 darwin/arm64 的 agent，由 agent 自己（`gpuworks-agent install-service`）注册 launchd
  任务而不是 systemd：有管理员权限时（`sudo` 会在终端里要密码）是 system 域的 LaunchDaemon `ai.gpuworks.agent`，
  以执行安装的用户身份运行、开机即起，agent 从不以 root 运行；没有时是该用户的 LaunchAgent，安装到 `~/.gpuworks`，
  只在该用户登录期间运行——用户没有登录会话（例如只通过 SSH 登录）时安装直接报错说明原因。plist 里只有
  `run --state <目录>`，节点 token 在该目录的 `gpuworks-agent.conf`（权限 0600）里，不出现在 `ps` 能看到的命令行或 plist 上。安装等到 launchd 稳定地把 agent 跑起来才算成功；
  重装时 launchd 拒绝新任务会恢复原来的任务，替换另一个 server 的 agent 会给出警告。
- SSH 向导同样可以接入 Mac：它上传 darwin/arm64 的 agent，在 Mac 上执行同一份安装脚本（不需要 Mac 能访问 server
  下载；Linux 主机的 SSH 安装现在也是同样的做法：把 `install.sh` 与 agent 上传到安装目录，用 `GPUWORKS_AGENT_BINARY` 指向上传的二进制执行，再删掉脚本，没有单独的 SSH 启动命令）。用账户密码登录时，这个密码也用于 `sudo`，agent 装成以该用户身份运行的 LaunchDaemon；用密钥登录又没有免密
  sudo 时装成 LaunchAgent，只在该用户登录期间运行，预检查会提示。预检查在 Mac 上检查的是 Apple silicon、macOS 版本
  与芯片，不找 GPU 厂商工具和 FUSE。
- 「升级 Agent」在 Mac 上同样原地替换并由 launchd 拉起，新版本未能连上 server 时自动回滚。
- agent 启动时把安装目录（模型、环境包都在其中）标为不参与 Time Machine 备份（`tmutil addexclusion`，随目录走，不需要管理员权限）；模型目录另设在别处时一并标记。
- Mac 之间可以互相同步模型与环境包（节点间同步）。代理软件开着 TUN 增强模式（Clash、Surge、sing-box 等）时，
  agent 上报的是网卡（如 en0）自己的地址，而不是代理的 198.18.x.x 虚拟地址。
- agent 在 Mac 上用 `host` 运行时：引擎是宿主机进程，因为 Linux 容器在 macOS 上跑在没有 Metal 的虚拟机里。
  镜像是 `gpuworks-runner:metal…` 的 darwin 环境包，从镜像目录自动选中；Docker Desktop 装了也不会被用。
- 节点显示 1 张统一内存的卡，显存是 Metal 允许 GPU 常驻的部分（默认约为内存的 2/3–3/4，由
  `iogpu.wired_limit_mb` 决定，平台不修改）。一台 Mac 可以同时跑多个实例，按各自的内存份额准入；
  vLLM 的份额是 `gpu_memory_utilization` × 显存，想并跑就调低它。
- 「终端」照常可用：节点终端是 agent 用户的 zsh；实例终端进入该实例的环境（环境包的 PATH、实例自己的 HOME 与工作目录），没有容器隔离，关闭标签页时会回收其中启动的进程。
- 不支持引擎休眠、多机推理与 KV 磁盘层，部署页会给出原因。有实例运行时 Mac 不会空闲睡眠，合盖仍会睡。
- 引擎只监听 127.0.0.1，开着 macOS 应用防火墙也不会弹窗。

## 安装目录检查

agent 的二进制、runtime 的 PRoot、解包后的镜像和模型缓存都放在安装目录（默认 `/opt/gpuworks`，向导「高级」里可改）下，
所以它背后的文件系统直接决定节点能不能用、怎么用。SSH 向导的预检查按你填的安装目录（留空即默认目录）跑一次探测，
SSH 安装在安装目录确定之后（含回退到 `/tmp/gpuworks` 之后）再跑同一个探测，结果追加到安装报告；云主机一键安装按
安装弹窗「高级」里的安装目录（留空即默认目录），同样会跑。目录还不存在时检查它最近的已存在上级目录，不会创建目录。探测结果是六行，名称固定，各自独立显示（详情只写文件系统类型和挂载点，不带挂载选项——容器根目录的 overlay 选项会列出每一层镜像路径）：

| 检查项 | 状态 | 含义 |
|---|---|---|
| Install directory | 通过 / 失败 | 报告实际探测的路径、文件系统类型、挂载点和挂载选项、剩余空间。**失败**：挂载为 `noexec`（agent 和 PRoot 都要从这里执行），或不可写（root 也写不进时说明是只读挂载或会把 root 映射成普通用户的网络文件系统；非 root 时指没有免密 sudo），或不是绝对路径（不跑探测）。`noexec` 时 SSH 安装直接失败，不会上传二进制；其余情况只报告，不阻止安装（不可写时，安装目录留空的安装会回退到 `/tmp/gpuworks`，重启即丢；填了安装目录的安装失败） |
| Install directory ownership | 通过 / 警告 | root（或经 sudo）在目录里建一个临时文件，试着 `chown 65534:65534` 后删除。文件系统拒绝改属主（例如 RunPod 的 `/workspace` 是 NFSv4，root 也报 `Operation not permitted`）时**警告**：udocker 工作负载会以 agent 自己的用户（root）运行，而不是专用的 `udocker` 用户，详情引用 chown 的原始报错。不是 root 也没有免密 sudo 时不检查 |
| Install directory filesystem | 通过 / 警告 | 网络或用户态文件系统（nfs、nfs4、cifs、smb3、ceph、glusterfs、lustre、fuse.* 等）**警告**：模型加载和引擎启动速度取决于它的吞吐和延迟 |
| Install directory free space | 通过 / 警告 | 剩余空间低于 50 GiB **警告**（一个引擎镜像解包约 10–20 GiB，再加一个中等规模模型的权重，50 GiB 是勉强放得下一套并留有余量的下限）。详情给出剩余和总量 |
| Install directory file names | 通过 / 警告 / 失败 | 在私有临时目录里试建一个文件名带 `:` 的文件。文件系统拒绝（例如 RunPod 的 `/workspace` 报 `Invalid argument`）时**失败**：容器镜像离不开这种文件名（udocker 的镜像层都叫 `sha256:<hex>`，Debian 系镜像里有 `/var/lib/dpkg/info/<包>:amd64.list`），没有 docker 的节点用 udocker 运行时，在这个目录里跑不了任何部署。请换一个本地文件系统上的安装目录（容器实例上就是容器盘上的路径，容量要放得下解包后的镜像）。节点装有可用的 docker 时只是**警告**（镜像由 dockerd 保存，不放在这里）；不阻止安装。不可写时不检查 |
| Install directory persistence | 通过 / 警告 | 在容器里（`/.dockerenv`、`/run/.containerenv` 存在，或接入时标明是容器实例）而目录落在 `overlay` 上，**警告**：容器可写层在实例重建时丢失，连同 agent、节点身份和模型缓存。实例有本地文件系统的持久卷时请把安装目录放到那里；RunPod 的 `/workspace` 不允许文件名带冒号（见上一行），放不了 udocker 运行时的镜像。目录在 `tmpfs`/`ramfs` 上也会警告。云厂商一键安装的目录是固定的（容器盘是 agent 在云容器实例上唯一能放的位置，厂商的网络卷放不了 runtime 的镜像），所以这一路的警告只写明“实例删除或重建就丢，换实例后重装”，不建议换目录 |

探测本身跑不起来（超时、输出不完整）只给出一条「Install directory」警告，不当作目录有问题。

### 为什么 udocker（runveil）节点的安装目录不能放在网络卷上

没有 docker 的节点（云容器实例常见）用 udocker 运行时，由 runveil 启动容器。它把镜像层、解包后的容器根文件系统
和启动所需的状态都放在安装目录下的 `udocker/`、`udocker-state/` 里，而不是交给某个守护进程管理。所以安装目录
所在的文件系统必须能像本地盘一样存放镜像内容：

- **文件名要允许冒号**。udocker 的镜像层按 `sha256:<hex>` 命名，几乎所有 Debian/Ubuntu 镜像里还有
  `/var/lib/dpkg/info/<包>:amd64.list` 这样的文件，容器里看到的必须是原名，没法改写。RunPod 的 `/workspace`
  （VAST 存储上的 NFSv4）对这种文件名返回 `Invalid argument`，镜像既拉不下也解不开。agent 初始化时会探测，
  不满足就直接报错（见[排障](troubleshooting.md)），预检查的 `Install directory file names` 也会标为失败。
- **最好允许改属主**。root 运行的 agent 让工作负载以专用用户 `udocker` 运行，需要把目录交给它；文件系统拒绝
  `chown`（NFS 常见）时会改为以 root 运行，不影响使用，但工作负载的身份变了。
- **速度取决于存储**。解包镜像和启动引擎要读写大量小文件，网络文件系统明显比本地盘慢。

因此在云容器实例上，安装目录请放在容器盘（本地 overlay 层，例如默认的 `/opt/gpuworks`），租机时把容器盘开到
放得下解包后的镜像和模型（一个推理引擎镜像解包约 20–40 GiB）。代价是容器重建后 agent 和镜像缓存都会丢，需要
重新接入、重新拉镜像；这也是 `Install directory persistence` 给出警告的原因。有 docker 的节点镜像由 dockerd
管理，不受这些限制。

### 加速卡设备权限（非 root 安装）

runveil 以 agent 的身份打开加速卡设备节点（NVIDIA 是 `/dev/nvidiactl`、`/dev/nvidia-uvm*`、`/dev/nvidia-modeset`、`/dev/nvidia<N>`，
昇腾是 `/dev/davinci*` 与 `devmm_svm`、`hisi_hdc`，PPU 是 `/dev/alixpu*`，海光是 `/dev/kfd`、`/dev/mkfd` 与 `/dev/dri/*`）。
root 运行的 agent（root 或免密 sudo 安装都是这种）自建专用用户 `udocker`，每次启动工作负载时附上设备节点所属的组，
不需要额外授权。既不是 root 也没有免密 sudo 时 agent 以登录用户自己的身份运行，预检查的 `Accelerator device access`
会按节点的厂商只检查上面这些设备（NVIDIA 主机上属于显示栈的 `/dev/dri/renderD*` 不会交给工作负载，不检查），逐个看能否读写：有读写不了的就**警告**，列出这些设备与所属组，以及管理员要执行的
`sudo usermod -aG <组> <用户>`（重新登录后生效）。这只影响 runveil：Docker 以 root 打开设备，所以只是警告、不阻止安装。
NVIDIA 节点上 Docker 没有 NVIDIA Container Toolkit 支持时 agent 也会改用 runveil（见[排障](troubleshooting.md)）。

## 一台机器插了多个厂商的卡

一个节点同一时间只管理一种厂商的卡。agent 启动时按昇腾、海光、PPU、NVIDIA 的顺序挑第一个能用的厂商（自动），
同时读主机的 PCI 总线，把总线上每个厂商的卡数与型号上报给 server（直通给虚拟机的卡不算）。只有确实看到不止一个
厂商的卡时，节点详情才出现 **加速卡** 一项：

- 下拉里是「自动（当前：昇腾）」和每个厂商的卡，例如「NVIDIA ×1（AD102 [GeForce RTX 4090]）」。
- 选自动且还有别家的卡时，下面有一行橙色提示：「这台机器还有 NVIDIA ×1（…）未纳管，可切换」。
- 选另一家后 agent 原地重启（同一进程重新执行自身），重启后探测、容器注入、指标、dGPU 探测、运行时检查都换成
  那家的；节点的卡换成那家的卡，原来那家的卡不再出现。重启完成前显示「正在切换到 …」。
- 节点上还有实例（未停止的）时拒绝切换（409 `NODE_ACCELERATOR_HAS_INSTANCES`），先停止或迁走再切；选主机上没有的
  厂商回 400 `NODE_ACCELERATOR_NOT_ON_HOST`。

选择同时存在 server 和 agent 状态目录的 `accelerator-preference` 文件里，agent 每次启动先读文件，所以重启、
升级、带外重装后第一份上报就是选好的卡；文件丢了也会在连上后由 server 下发、节点空闲时补回。切到的厂商若容器运行时
不支持（例如 Docker 没装 NVIDIA Container Toolkit），自动模式的运行时会照常回退到 runveil 并给出提示。

## 重复接入同一台主机

所有安装都写同一个 systemd 单元 `gpuworks-agent`，所以给一台已有 agent 的主机再装一次，会替换掉原来的 agent：
原来的节点（不论属于本 server 还是另一个 server）就此离线。为此：

- **预检查**多两项：`Existing agent` 读出主机上 `gpuworks-agent` 单元和正在运行的 agent 各自所属的 server 地址
  （取自各 agent 的配置文件，较早安装的 agent 取命令行里的 `--server`；不读 token，也不打印命令行。Server 发起连接的 agent 没有 server 地址，显示为未知），有就给警告：单元会被替换，写明它属于本 server 还是哪个 server；单元之外
  手工启动的其他 server 的 agent 不受安装影响，只列出地址。`Existing node` 在本 server 已有节点的 SSH 地址
  与目标相同时给警告，并写出节点名。
- **SSH 安装**遇到会被替换的 agent（单元，或没有单元时本 server 手工启动的 agent）或同地址的已有节点，会先停下
  （还没改动主机），向导弹窗写明已有的节点名或单元所属的 server 地址，确认后才替换。
- **安装命令**（`curl … | sh`）和离线包不能交互确认，替换另一个 server 的 agent 时在 stderr 打一行 `WARNING`，照常安装。

替换后另一个 server 上的节点会离线，它的带外恢复会报 `foreign_agent`（见[排障](troubleshooting.md)），不会抢回。

## agent_dial 与 server_dial

`agent_dial` 是默认的节点→server 外连，入站端口为 0。只有 SSH 预检查确认反向可达时才用
`server_dial`。添加节点时选「Server 发起连接」，表单里有两个地址：

- 「Agent 监听地址」：agent 在这台主机（或容器）里监听的地址，默认 `0.0.0.0:18800`（`host:port` 或 `:port`；非法地址由 API 以
  `SERVER_DIAL_LISTEN_INVALID` 拒绝）。已有节点保留各自记录的监听地址，不随默认值改变。
- 「计算节点接入地址」（`host_address`，必填）：server 连接这台主机用的地址，IP、域名或平台给的映射地址。只填主机时端口取监听端口；
  平台把端口映射成别的号码时填映射后的 `host:port`，它的端口优先于监听端口。

Server 发起连接的 agent 不涉及 server 地址：安装不带 `--server`，API 不为这类节点保存 `server_url`（控制台发来什么都不存），
`gpuworks-agent run` 只需要 token 和监听地址。因此这类 agent 也无法自己下载升级：手工 `gpuworks-agent upgrade` 会拒绝，
升级由 server 经已建立的连接推送（节点页「升级 Agent」）。要确保 server 能访问监听端口：节点的安全组或防火墙要放行入站 TCP。

### 离线包

Server 发起连接的节点通常也下载不到 server 上的 agent，又未必有 SSH。有 SSH 时 SSH 向导把 agent 和安装脚本传上去；没有 SSH 时用离线包：

- 下载地址 `GET /dl/gpuworks-agent.run`，校验和在 `/dl/gpuworks-agent.run.sha256`（`sha256sum` 格式）。和其他 `/dl/` 文件一样由「节点接入」功能的监听地址提供（默认就是控制台地址，网关地址上没有），不需要登录，所以文件里**没有 token**，所有节点通用。
- 是一个自解压的 sh 脚本，内含 linux-amd64、linux-arm64、darwin-arm64 三个 agent（server 内嵌了哪些就带哪些）和 `install.sh`，运行时按 `uname` 选用，节点上需要 `tail`、`head`、`gzip`、`mktemp`。
- server 启动后第一次被请求时构建，存在 `<data_dir>/offline/`；server 升级重启后重新构建，不会提供旧版本的包。
- 用法：把文件传到节点（scp、Web 终端或 JupyterLab 上传均可），在它所在的目录执行：

```sh
sh gpuworks-agent.run --token '<节点 token>' --connect-mode server_dial --listen '0.0.0.0:18800'
```

参数就是[安装脚本的参数](../reference/cli.md#节点安装脚本-installsh)。添加节点抽屉里，选「Server 发起连接」且主机没有 SSH 时，页面给出下载链接、sha256 和这条命令；节点详情里给无法带外恢复的节点生成的恢复命令，对 Server 发起连接的节点也是同一个形式。

节点之间互相访问用的是**管理网地址**：agent 连 server 所用的本机地址，自动上报，节点详情页可见。
节点间同步、多机推理和服务流量怎么选网络，见[配置节点网络](configure-networks.md)。

## 节点与 server 网络不通

预检查从两个方向检查 server 与节点之间能不能连：

| 检查项 | 方向 | 含义 |
|---|---|---|
| Server reachability | 节点 → server | 在节点上请求 `server_url` 的 `/healthz`（Agent 发起连接时检查）。失败说明节点访问不到这个地址 |
| Server to agent | server → 节点 | server 按它实际会拨的地址发起 TCP 连接：「计算节点接入地址」自带的端口优先，否则用监听端口（Server 发起连接的 `--listen`，默认 18800）。安装前那里还没有程序在监听，所以“连接被拒绝”也算通过——说明数据包到了节点又回来了；超时说明被防火墙或安全组拦截，或者没有路由。Server 发起连接时，这个地址不通是**错误**，详情写明安全组或防火墙丢弃了该端口的连接，要为 server 放行入站 TCP 端口 `<端口>`。Agent 发起连接时，只有节点访问不到 server 才显示这一行（只是警告） |
| Connectivity | 结论 | 选定的方式走不通时给出：另一种方式能通时提示改用它；两个方向都不通时警告，并指向本节 |

server 一侧中间有透明代理时，任何 TCP 连接都会“成功”，`Server to agent` 可能误报通过。

两个方向都不通时 agent 装上也连不上，常见于云上没有公网 IP 的实例（例如阿里云 VPC 里没有绑定 EIP、也没有 NAT 网关的 ECS）。按 server 在哪里选一种办法：

| server 所在位置 | 办法 |
|---|---|
| 公网（或有公网入口） | 让节点能出公网：给实例绑定 EIP，或在 VPC 里配置 NAT 网关（SNAT），然后用 server 的公网地址作为 `server_url` |
| 与节点在同一 VPC | 直接用 server 的私网地址作为 `server_url` |
| 其他私有网络（机房、另一个 VPC） | 打通两边网络：VPC 对等连接、云企业网（CEN）或 VPN 网关，然后用打通后可达的地址 |
| 任意位置，不想改网络 | 在节点所在 VPC 里放一台能同时访问节点和 server 的机器做反代（例如 Nginx，配置见[生产环境部署](deploy-production.md#反代配置要点控制台)），`server_url` 填它的私网地址 |

节点能访问 server 但 server 访问不到节点（节点在 NAT 后面）时，用 Agent 发起连接即可，不需要处理。

不能出公网的节点装好 agent 后，仍然不能从公网下载镜像和模型，导入办法见[离线环境部署：节点不能出公网](deploy-offline.md#节点不能出公网)。

## 升级

节点页“升级 Agent”触发的自升级由 server 经节点已有的连接把新二进制推给 agent（较早的 agent
仍自己从 `/dl/` 下载并跑 `smoke`），校验通过才原子替换并用新二进制 `exec`，校验失败保留旧二进制
不被覆盖。新版本要在确认期限内被 server 重新连上才算成功，否则自动回滚到 `<bin>.old`，细节见
[升级与版本兼容](upgrade-and-compatibility.md)。`gpuworks-agent upgrade` 命令仍走下载 + `smoke`。
重新运行 `/install.sh` 是另一条路径：下载到 `<bin>.tmp.$$` 后直接替换，不做 smoke 校验。手工检查
`agent.log` 和节点版本，确认新版本重新上线后再关闭旧节点。

## 维护节点：停止调度与驱逐

升级驱动、换卡或下架一台节点前，在节点列表该节点的“更多 → 调度与驱逐”里操作：

- **停止调度**：不再往这台节点启动新副本，已有副本照常服务。适合短时维护、观察。
- **驱逐**：停止调度，并把从候选池放过来的副本逐个迁到别的节点——每个部署一次迁一个，先在别处起好替代副本，
  替代副本运行后原副本才退役，服务副本数不低于驱逐前。弹窗列出节点上还剩哪些副本、驱逐会怎么处理它们，每 5 秒刷新。
- **强制驱逐**：钉在这台节点上的部署、多机与 PD 的 rank 不会被驱逐迁走；需要立即腾空节点时点“强制驱逐”，
  它们立即撤下（不计入重试次数），能放到别处的在别处重建，指定了这台节点的等它恢复调度。也可以先在部署详情里
  “编辑”节点条件、或在“调整副本”里换节点，让它们正常迁走。
- **恢复调度**：维护完成后恢复，等待中的副本立即重试。

节点下线也会让副本在别处重建，但那是故障转移：副本先失败、扣重试次数，期间服务能力下降。计划内的维护用驱逐。

## 卸载

先停止节点上的部署实例，再从节点列表选择「更多 → 删除」。普通删除使用已保存的 SSH 凭据执行
`gpuworks-agent uninstall`，确认卸载后才删除管理记录；SSH 通道独立于 agent，agent 停止不影响卸载回复。
要卸载的 agent 按节点 token 认定：同一台主机上跑着多个 agent 时只停这个节点的那一个，其他节点的 systemd 服务与
nohup 进程保持运行。
SSH 不可达、凭据缺失或卸载结果无法确认时，保留节点和 SSH 凭据，可修复后重试。

无法联系节点时，可以明确勾选「强制移除：仅删除管理记录」。这会跳过远程卸载，agent 可能仍在运行，
恢复节点访问后需手动卸载。强制移除也不能删除仍有实例的节点。节点上的旧版 agent 若无法停止
nohup 进程，普通删除会失败；先升级 agent 或手动卸载确认后，再强制移除记录。

也可以直接在节点上执行：

```bash
gpuworks-agent uninstall --state /opt/gpuworks
```

Linux 命令停止并移除 systemd unit，同时停止相同二进制及显式 `--state` 目录的 `run` 进程（包括 nohup），
确认退出后删除 `identity.json` 和 PID 文件；失败以非零状态退出。macOS 移除 launchd 任务，
LaunchDaemon 需用 `sudo` 执行，state 目录取自任务本身。二进制、模型、日志和 state 目录保留。需要彻底
清理数据时，先确认没有实例或模型仍在使用，再由管理员手动删除 state 目录。
