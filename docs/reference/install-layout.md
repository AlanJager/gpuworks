# 控制面安装布局

本页查表用：`gpuworks install` 产生的默认值、目录与文件权限、凭证文件、systemd 单元、备份内容和交互提问的默认值。安装、升级、卸载的步骤见[安装和升级控制面](../how-to/install-server.md)，命令行参数见 [CLI 与安装脚本](cli.md#gpuworks-install)，`server.yaml` 的键见[配置参考](configuration.md)。

## 默认值

| 项目 | 默认 |
|---|---|
| 安装目录 | `/opt/gpuworks/server`；macOS 上是 `~/gpuworks/server`（执行安装的用户的主目录，`sudo` 下取 `SUDO_USER` 的） |
| 监听地址 | `<本机默认路由出口 IP>:8800` |
| 数据目录 | `<dir>/data` |
| 日志 | `<dir>/logs/gpuworks-server.log`，10 MB × 20 代 |
| 备份 | `<dir>/backups`，保留最近 3 组 |
| 服务 | systemd 单元 `gpuworks.service`，以系统用户 `gpuworks` 运行；macOS 上默认不注册服务，以 nohup 后台进程运行（`serve --supervise`），加 `--autostart` 才写 launch daemon `/Library/LaunchDaemons/ai.gpuworks.server.plist`，以执行安装的用户运行 |
| 受信代理 | `127.0.0.1/32,::1/128` |

安装目录是 agent 默认目录（Linux 上 `/opt/gpuworks`，macOS 上 `~/gpuworks`）的子目录：同机同时装控制面和 agent 时，两者的二进制、state 和数据互不覆盖。

`--dir`、`--data`、`--logs` 以及 `install directory` 提问的回答中，开头的 `~`、`~/x`、`~user/x` 展开为对应用户的主目录。

监听地址取本机默认路由出口 IP、端口 8800；找不到默认路由时取第一个非回环、非虚拟网卡的 IPv4。`--addr` 只写 `:端口` 表示绑定 `0.0.0.0`。

## 目录布局

```
/opt/gpuworks/server/
├── gpuworks                        # 安装的二进制（install 拷贝自身，upgrade 原子替换）
├── server.yaml                     # 配置文件：监听地址、数据目录、日志、受信代理等
├── .upgrade.lock                   # 升级互斥锁
├── gpuworks.pid                    # 仅 nohup 模式
├── tmp/                            # 升级时暂存下载包与解出的新二进制
├── backups/<release_id>-<UTC时间戳>.tar.gz
├── logs/gpuworks-server.log[.1..20]
└── data/
    ├── gpuworks.db(+ -wal / -shm)  # 主库
    ├── server.lock                 # serve 存活期间持有的排他锁，内容是其 pid
    ├── master.key                  # 凭证加密密钥
    ├── initial-credentials         # 初始密码记录
    ├── install-history.jsonl       # 安装/升级审计
    ├── plugins/                    # 插件包：<id>/pkg/<版本>/（只读）、<id>/pkg/current、<id>/control.json、<id>/data/ 等
    └── logs.db / metrics.db / ux.db / images/ / catalog-packages/ …
```

`data/plugins/` 由 `gpuworks plugin` 管理：包先解到 `.incoming-*.partial/` 里校验，全部通过才改名为 `<id>/pkg/<版本>/`（文件 0444、可执行文件与目录 0555），再以改名替换 `<id>/pkg/current` 切换版本，上一个版本保留以便回滚；中断留下的 `*.partial` 在下次安装时清掉。`<id>/data/`、`<id>/tmp/`、`<id>/snapshots/` 是插件自己的目录（0700），卸载时保留，`--purge` 时整个 `<id>/` 移到 `.trash/<id>-<时间>/`；升级前插件停止后，数据目录复制到 `<id>/snapshots/<时间>-<版本>/`（`data/` 与 `snapshot.json`：来源版本与当时的数据标记），先写成 `.partial` 再改名，只保留最近 2 个，清除时随 `<id>/` 一起移走；升级进行中有 `<id>/upgrade.json`（`from`、`to`、`snapshot`、`started`，0600），升级通过健康门或回滚后删除，启动时还在就先回滚到 `from`（见[扩展点](../dev/extensions.md#管理接口)），卸载时删除。`.index/<发布者>/` 存导入的包索引。`maintenance.jsonl` 是离线维护日志（0600，属主为数据目录的所有者）：root 在 server 停止时执行 `gpuworks plugin clock reset` 或 `pstate clear` 各追加一行，server 下次启动时应用并清空，应用失败的记录留到再下次启动（见 [CLI 参考](cli.md#维护日志)）。

声明了 `host_setup` 的包在数据目录之外还有 root 写的文件（见[扩展点的主机配置](../dev/extensions.md#主机配置)），由 `gpuworks plugin host-setup`、`gpuworks install`/`upgrade` 写入，`gpuworks uninstall` 删除：

| 路径 | 内容 |
|---|---|
| `/etc/udev/rules.d/70-gpuworks-plugin-<id>.rules` | 每包一个 udev 规则文件（0644），按包声明的厂商与产品号匹配 USB 设备，把它的 `/dev/bus/usb` 节点与 hidraw 节点设为组 `gpuworks-usb`、模式 0660；不建符号链接 |
| `/etc/gpuworks/plugins/<id>.applied` | 已应用记录（0644，属主 root）：root 应用的主机步骤的摘要。server 只认 root 所有、组与其他用户不可写的记录，服务账号拥有插件目录也伪造不了 |
| 系统组 `gpuworks-usb` | 服务的设备组：`install`/`upgrade` 写进服务单元的 `SupplementaryGroups=`，`plugin host-setup` 把服务账号加进组；卸载时保留 |

这些路径是全机的、只按包 id 命名：`install`、`upgrade`、`uninstall` 与 `--prune` 按本安装的数据目录对齐，会删掉同机另一个安装写的文件，所以一台主机只让一个安装应用主机配置。

`data/server.lock` 由 `gpuworks serve` 在整个生命周期里持有排他 flock，并写入自己的 pid。同一数据目录上的第二个 server 等待 10 秒后拒绝启动并报出持有者的 pid；`plugin uninstall`、`enable`、`disable` 这类会被运行中 server 的内存状态悄悄覆盖的离线命令拿不到锁就拒绝（见 [CLI 参考](cli.md#gpuworks-plugin)）。锁随进程走，进程崩溃时内核自动释放；文件本身留着不代表 server 在运行，里面的 pid 也可能属于已退出的 server：报出 pid 前先确认该进程还活着，否则只点名锁文件。只有本版本起的 server 会持有这把锁，滚动升级时更早版本的 server 运行期间离线命令照样拿得到锁，须先自行停掉它。

`server.yaml` 放在安装目录而不在数据目录：把数据库恢复到另一台机器时，不会带上旧机器的监听地址。

`server.yaml` 和安装目录本身是重复安装和升级的输入：

- `upgrade` 从既有安装中读取升级所需信息，因此不需要重复提供安装参数：
  - **安装目录**：使用 `--dir` 指定的目录。未指定时，使用当前执行的二进制所属的安装目录；无法确定时使用默认目录。
  - **服务方式**：检查是否存在指向本二进制的 systemd 单元或 launchd plist。
  - **服务用户**：读取 systemd 单元的 `User=` 或 plist 的 `UserName`。
  - **当前版本号**：查询已安装的二进制。
- 重复执行 `install` 时保留既有 `server.yaml` 的值，只有命令行显式给出对应参数才覆盖。
  install 只写监听地址、数据目录、日志文件、日志轮转和受信代理这几项；不管理的键（如 `ssh_proxy`、`metrics_retention`）原样保留。

### 控制面装在 agent 共享根目录时

默认独立目录不变。既有控制面若直接装在 `/opt/gpuworks`，修复安装或执行实际升级时，将配置迁移到 `/opt/gpuworks/server-config/server.yaml`，并保留 `/opt/gpuworks/server.yaml` 指向它的受控相对链接。服务使用实际配置路径；二进制、PID、升级锁、备份和相对路径的工作目录仍属于原安装目录。

共享根目录保持 root 所有且普通服务不可写；只把专用配置目录交给控制面服务，网页设置仍以临时文件加原子替换保存，不向服务开放 agent 的二进制、辅助库或缓存目录。迁移保留既有配置注释和未管理的键。任意链接、冲突的私有文件或悬空链接会被拒绝，不会当作迁移入口。

旧版安装器曾把整个 `/opt/gpuworks` 交给服务账户（属主 `gpuworks:gpuworks`、`2775`）。以 root 或可用 `sudo` 执行修复安装或实际升级时，先把这个目录本身改为 `root:root`、`0755`（不带 setgid），每项改动打印一行，并在安装历史中记一条 `action: layout` 记录（`layout_changes` 列出路径及改动前后的 uid、gid 和模式，配置迁移另记一项）。迁移只改共享根目录本身：`server-config/`、升级锁、`tmp/`、`backups/`、数据和日志目录照常交给服务账户；共享根目录中的二进制由 root 所有、模式 `0755`（root 会执行它：`sudo gpuworks upgrade` 和服务启动前以 root 运行的钩子），不交给服务账户。迁移前服务账户可以替换目录里的任何条目，所以递归改权限之前逐项检查服务的条目：不是 root 或当前操作员创建的符号链接（数据或日志目录可以是 root 建的、指向别的磁盘的链接）、类型不对的条目，以及硬链接数大于 1 的文件都会被拒绝，报错指出路径；`server.yaml` 是任意链接或硬链接时同样拒绝迁移。既不属于服务、也不归 root 的其他条目保持原样，每项打印一条警告并记入同一条 `action: layout` 记录（`foreign_entry_kept`，带属主和模式），由操作员确认来源；旧安装器也曾把同机 agent 的文件交给服务账户。目录属主须为 root、当前操作员或安装的服务账户，且其他用户不可写，路径不能是符号链接，父目录须可信，带额外 ACL 时拒绝；不满足时只报错不改。没有 root 也没有 `sudo` 时拒绝执行，报错给出具体命令：以 `sudo /opt/gpuworks/gpuworks upgrade` 重试，或手工执行 `sudo chown root:root /opt/gpuworks && sudo chmod 00755 /opt/gpuworks`（GNU `chmod` 对目录只在多写一位前导 0 时清除 setgid，`chmod 0755` 会留下 `2755`）。`upgrade --check` 和没有版本变更的升级只读，不迁移。升级失败回滚时恢复旧二进制、数据库和配置，目录保持迁移后的形态，旧版本在此形态下照常运行；回滚不会把目录改回 `2775`，那样会重新让服务账户控制 root 执行的文件。迁移中途失败（例如属主已改为 root、模式仍是 `2775`）时重新执行同一命令即可完成。

迁移后服务账户不能在共享根目录中替换二进制，控制台升级显示为不可用（`not_writable`），升级改用命令行 `sudo /opt/gpuworks/gpuworks upgrade`。

在共享根目录中，命令行升级不经过服务账户的 `tmp/` 暂存新版本：下载包和解出的二进制放在只有执行升级的账户可进入的 `/opt/gpuworks/.upgrade-staging/`（`0700`）中。移动到位前核对暂存目录仍是本次创建的私有目录、二进制仍是解包时的内容（SHA-256），不符时拒绝替换并回滚；随后先把它改为 `root:root`、`0755`，移动后对安装的二进制再设一次，然后才启动服务。暂存目录在升级结束时删除；若它已存在且不是 root 或当前操作员的私有目录，升级拒绝执行。非 root 账户不能在 root 所有的共享根目录中创建暂存目录，因此这里的命令行升级须以 root 执行。

`upgrade --check` 和没有版本变更的升级不迁移文件。若当前运行的旧安装器尚不包含此修复，应使用新发布包里的二进制对原目录执行修复安装。备份包中的配置成员仍叫 `server.yaml`；普通卸载保留配置和兼容链接，只有 `--purge` 删除受控配置及链接，不递归删除共享根目录。

macOS 若将控制面自定义安装到这个共享根目录，清理配置前会确认当前进程具有原生恢复文件的权限；权限不足时拒绝清理并提示以 root 执行整条卸载命令，避免不安全的提权链接恢复。

## 权限模型

安装树是服务账户与操作员的共享工作区：

| 对象 | 权限 |
|---|---|
| Linux systemd 目录 | `2775`（组可写 + setgid，新文件继承平台组 `gpuworks`） |
| 普通配置文件（如 `server.yaml`） | Linux 共享安装为 `0664`；macOS 移除组写权限 |
| 数据库、`master.key`、`initial-credentials`、备份包 | 默认 `0600`；明确授权的 Linux 平台组使用 `0660` |
| 二进制 | `0755` |
| `data/install-history.jsonl` | `0600`（独立运行）或 `0660`（可信 Linux 服务组） |
| systemd 单元 / launchd plist | `0644`（plist 归 `root:wheel`，launchd 要求） |

- install 以**操作员身份**运行，只有少数步骤以单独的 `sudo` 命令执行：首次创建 `/opt` 目录、建服务账户、写 systemd 单元、`systemctl`，以及收尾时把树交给服务组。`sudo` 在第一次特权命令时问一次密码，之后凭据缓存期内（通常 15 分钟，取决于主机 sudoers 配置）免问。
- 首次安装时树内的写入（二进制、配置、凭证）还无法直接落盘，会自动走 `sudo` 兜底，收尾统一 chown；之后的安装和升级都直接写入。
- systemd 模式下操作员会被加入平台保留组 `gpuworks`，因此之后 `reset-password` 不需要 `sudo`，`upgrade` 的备份与替换二进制也不需要；`upgrade` 仍会用 `sudo` 执行 `systemctl` 以及收尾的 `chown`/`chmod`。新组在**当前终端**不生效，需要 `newgrp gpuworks` 或重新登录。
- nohup 模式没有特权步骤：进程以调用者身份运行、目录归调用者，唯一例外是首次创建 `/opt` 目录。nohup 进程归安装者——root 装的只有 root 能用 `gpuworks restart --dir <安装目录>` 重启。
- 预检要求：root 或可用的 `sudo`（否则降级为 nohup 并告警；`sudo` 也没有且 `--dir` 不可写时拒绝安装，提示改用用户级目录）、目标端口未被别的进程占用（被当前安装占用除外）、安装目录所在文件系统至少 2 GiB 空闲。

敏感文件默认仅所有者可读写（`0600`）。明确绑定本安装二进制和非 root 服务账户的 Linux systemd 安装可使用可信服务组（`0660`）；该组成员等价于拥有平台凭据访问权。macOS 与独立运行默认不向普通用户组授权。启动、安装和升级会收紧存量凭据、数据库及其 sidecar、已识别备份；临时数据库快照仅所有者可读。无法保护文件时拒绝继续，按错误路径修复所有者与服务组后重试。生成口令不写入服务日志。

授权组固定为平台保留组 `gpuworks`，不跟随自定义服务账户的普通主组；systemd 单元显式写入 `Group=gpuworks`，文件实际 GID 必须与该组匹配。旧默认服务安装可保留已有 `gpuworks` 运维组授权；其他未知安装按仅所有者策略处理。回滚到未包含此修复的旧版本后，旧代码可能再次放宽文件权限，应重新升级修复版本。 预检发现旧文件需要收紧、但当前操作员没有所有者权限时，会在停服前拒绝安装或升级；以文件所有者或 root 重试。提权写入仅用于经过校验的可信目录，目录校验失败时也应以所有者或 root 运行完整命令。

Linux 的组授权会先清理敏感文件上的 POSIX 扩展 ACL。为避免额外授权通过文件替换或 sidecar 继承生效，共享数据库目录、提权写入的可信目录，以及安装收尾将授予组权限的敏感目录链，不接受扩展或默认 POSIX ACL；若错误指出 ACL，应先检查并清理不必要的 ACL，再重试。

## 凭证文件

单元文件是全局可读的，所以初始密码**不会**写进单元文件，而是走 `<data>/initial-credentials`（JSON，`0600` / `0660`，明文）：

```json
{"user": "admin", "password": "...", "created_at": "2026-09-10T09:00:00Z"}
```

install 时的密码优先级：

1. `--password`；
2. 环境变量 `GPUWORKS_ADMIN_PASSWORD`（交互模式下回显并确认，拒绝则继续提问）；
3. 交互模式提问（关闭回显，回车则随机生成；已有 admin 账户时不提问）；
4. `--quiet` 或 stdin 不是终端时，随机生成。

服务首启时创建 admin 账户的优先级是 `GPUWORKS_ADMIN_PASSWORD` > `initial-credentials` 文件 > 随机生成（不经 install、直接 `gpuworks serve` 时可用环境变量指定初始密码），并把最终使用的密码写回该文件。`GPUWORKS_ADMIN_PASSWORD` 只在创建 admin 时读取。该文件记录的账户修改密码后（控制台或 `reset-password`），server 或命令会尝试删除该文件（其中的密码已失效）；删除失败只记告警，需手动删除。记录的是其他账户时不删除。

数据目录已有 `gpuworks.db` 时视为已有 admin 账户：不再解析密码，数据库和账户不会被改动，`--password` 被忽略并提示改用 `gpuworks reset-password`。

## install 的交互提问

输出是终端且未给 `--quiet` 时，install 先打印 banner（终端且 locale 为 UTF-8 时用块状版本，否则纯 ASCII），再逐个提问，回车采用括号里的默认值：

| 提问 | 新安装的默认 | 已有安装时的默认 | 命令行已给出时 |
|---|---|---|---|
| `install directory` | `/opt/gpuworks/server`；macOS 上为 `~/gpuworks/server` | 同左（由 `--dir` 决定目录） | `--dir` 跳过 |
| `listen address` | 探测到的 `<IP>:8800` | `server.yaml` 里控制台监听地址的第一项 | `--addr` 跳过 |
| `start the service automatically` | `yes`；macOS 上为 `no` | 原安装是 nohup 时为 `no`，否则 `yes`；macOS 上原安装有 launch daemon 时为 `yes`，否则 `no` | `--autostart` 或 `--no-systemd` 跳过 |
| `Admin password`（关闭回显，最后一步） | 随机 | 数据库已有 admin 时不问 | `--password` 或 `GPUWORKS_ADMIN_PASSWORD` 已确认时不问 |

- 监听地址只输入端口号时保留默认里的地址、只替换端口；其他输入必须是 `IP:端口`，否则重新提问。
- 自启问 `yes` 等价于 `--autostart`，`no` 等价于 `--no-systemd`，回答只认 `yes`/`y`/`no`/`n`。
- `--quiet` 或 stdin 不是终端时不打印 banner、不提问，全部按默认值走；结束时的摘要仍会打印（里面有管理员凭证）。

## systemd 单元

服务单元通过 `server.yaml` 读取 server 配置，不在单元文件中写入这些配置值或密码：

- `ExecStart=<dir>/gpuworks serve --config <dir>/server.yaml`；nohup 模式跑的是同一条命令行，日志、数据和探活行为一致。
- `User=` 为 `--service-user` 指定的服务账户；`Restart=always`、`RestartSec=3`；`TimeoutStopSec=30`，给进行中的模型与镜像同步留出优雅退出时间。
- `KillMode` 保持默认（control-group）：server 不拥有推理容器，子进程应随它一起退出（agent 的单元则相反）。
- `Delegate=yes`：把单元的 cgroup 交给 server，server 把自己迁进其中的 `server` 子 cgroup，给每个插件建带内存、CPU 与进程数限制的 cgroup（[扩展点：跳板](../dev/extensions.md#跳板gpuworks-plugin-exec)）。命令行 `install` 与带核心安装钩子的 `upgrade` 重写单元并保留这一行；控制台升级不重写单元，未委派 cgroup 时由 RSS 看门狗限制插件内存。
- drop-in `/etc/systemd/system/gpuworks.service.d/host-identity.conf`（0644，只在 systemd 模式下由命令行 `gpuworks install`、`gpuworks upgrade` 写入，`uninstall` 删除；独立运行与 launchd 模式没有它）：`ExecStartPre=-+/etc/systemd/system/gpuworks.service.d/host-identity-exec host-identity --write /run/gpuworks/host-identity.json --group gpuworks`（服务账户为 root 时不带 `--group`）。`+` 让这一步以 root 运行，`-` 让它失败时 server 照常启动，只是读不到 root 才能读的两项。它写出的 `/run/gpuworks/` 目录为 `root:root`、0755，不允许额外的访问 ACL 或默认 ACL，`host-identity.json` 为 `root:gpuworks`、0440：服务账户能读、不能改也不能替换；该目录有额外访问或默认 ACL 时拒绝写入，避免 0440 激活继承的第三方读取权限；`/run` 是内存文件系统，每次开机重写。安装目录含空格或 `%` 时按单元 `ExecStart` 的同样规则加引号；含 `$` 或换行的路径拒绝。控制台升级以服务账户运行，不写 drop-in：只经控制台升级、从未以 root 执行过本版本命令行 `install`/`upgrade` 的安装没有这一步，系统 UUID 与主板序列号一直读不到，也不报错（`-` 忽略失败）；以 root 再执行一次 `gpuworks install`（重复安装）或下一次命令行 `gpuworks upgrade` 即补上。命令行升级因此每次都重写 `gpuworks.service` 并 `daemon-reload`，让新的 drop-in 在重启时生效，手工改动单元文件会被覆盖，自己的改动请另写 drop-in。见 [`gpuworks host-identity`](cli.md#gpuworks-host-identity)。 root 执行的是同目录中的 `host-identity-exec` 副本，不是服务账户可替换的主程序。命令行安装/升级从本次正在执行的安装器映像原子发布该副本；副本与全部路径祖先必须由 root 所有、不可被组或其他用户写入，且不允许符号链接或额外 ACL。控制台升级保留上次可信副本，不从主程序目录刷新；后续版本须兼容已有身份文件格式。卸载只删除本功能的副本和 drop-in，保留其他 drop-in。

只有在有可用 root/`sudo`、`systemctl` 存在且 `/run/systemd/system` 真的存在（容器里装了 systemctl 但没有 systemd 不算）时才写单元；macOS 上只在 `--autostart`（或提问回答 `yes`、重装已有 launch daemon 的安装）时写下面的 launchd plist；都不满足时使用 nohup 后台进程加 PID 文件。

## launchd plist（macOS）

macOS 上 systemd 单元的对应物是 system 域的 launch daemon `/Library/LaunchDaemons/ai.gpuworks.server.plist`，同样只指向 `server.yaml`、不含设置或密码：

- `ProgramArguments` 与单元的 `ExecStart` 相同；`UserName` 是执行安装的用户（`sudo` 下取 `SUDO_USER`），不是 root，也不建专用账户，`--service-user` 在 macOS 上被忽略（显式给出别的账户时告警）；只有 root 本身执行安装时以 root 运行。
- `RunAtLoad` + `KeepAlive`：开机即起、退出 3 秒后（`ThrottleInterval`）再拉起，对应单元的 `Restart=always`；`ExitTimeOut` 30 秒对应 `TimeoutStopSec`；不设 `AbandonProcessGroup`，子进程随 server 退出。
- 环境变量 `GPUWORKS_LAUNCHD=1` 告诉 server 它由 launchd 托管，因此可以在控制台升级、重启（退出后由 launchd 拉起）。
- `StandardErrorPath` 是 `<dir>/logs/gpuworks-server.stderr.log`，只收 server 日志打开之前的输出和崩溃时运行时打印的内容。
  正常日志记录写在轮转的 `gpuworks-server.log` 里；`gpuworks-server.stderr.log` 超过轮转大小上限时，在启动时清空。
- 安装树归运行用户（`chown -R <用户>:<其主组>`），不加入任何组。

## 探活与备份

- install 与 upgrade 启动服务后轮询 `/healthz`，最长 60 秒；探测地址是监听地址对应的回环地址（通配地址用 `127.0.0.1`）。
- nohup 模式下这个等待会在进程已经退出时提前结束：该模式没有监管者，启动留下的进程一旦消失就不会再有人应答，继续等满只是推迟回滚（例如新版本缺少动态库、或拒绝某项配置而启动即退出）。systemd 单元与 launchd plist 都会拉起崩溃的 server，所以那里的 60 秒正是留给重试的，不提前结束。
- 备份是 `backups/<release_id>-<UTC时间戳>.tar.gz`，包含 `data/gpuworks.db`（含 `-wal` / `-shm`）、`data/master.key`、`server.yaml` 和 `bin/gpuworks`，不含模型、镜像等数据目录里的其他内容。恢复会先删掉旧库的 WAL/SHM，避免新版本写入的前滚日志被旧 schema 重放。
- 每次安装和升级向 `<data>/install-history.jsonl` 追加一行审计：动作、结果、release、操作者（`sudo` 下取 `SUDO_USER`）、时间；失败时含 `previous_release_id` 与错误。文件在把安装树交给服务账户之前创建，因此归服务账户所有，控制台升级由服务账户自己确认时也能追加。审计写不进去不会让安装或升级失败：控制台升级照常确认，服务日志记一条 `console upgrade confirmed without an install history entry` 警告。

## 日志与轮转

server 同时写 stderr 和文件：`<dir>/logs/gpuworks-server.log`，按 10 MB 轮转、保留 20 代（install 的 `--log-max-size` / `--log-keep` 可调；直接跑 `serve` 时对应 `--log-file`，传空串则只写 stderr）。轮转是原生实现，不依赖 logrotate；systemd 部署下 `journalctl -u gpuworks` 也能看到同一份输出。

launchd 部署下，stderr 指向的文件没有轮转机制。server 发现 stderr 是普通文件时，正常日志只写入可轮转的日志文件（见上节）。日志目录不可写时退回 stderr，不影响启动。

日志文件写失败时（例如磁盘已满）停写，记录仍写进 stderr。之后每 30 秒重试一次，成功后先写一行 `log file writes resumed`，带丢弃的条数（`dropped`）、停写的起点（`since`）和原因（`err`），不用重启。agent 的日志文件也是这样。

server 因错误退出（如监听地址被占用）时，原因以 `msg="server exited"` 写进同一份日志；nohup 模式的 stderr 指向 `/dev/null`，这是唯一能看到原因的地方。

## agent 配置文件

这一节说节点上的 agent，不是控制面。agent 的启动配置只记在一处：state 目录（默认 `/opt/gpuworks`，macOS 上是 `~/gpuworks`）里的 `gpuworks-agent.conf`，权限 `0600`，里面有节点 token。它只由 `gpuworks-agent configure` 写（安装脚本调用，见 [CLI](cli.md#gpuworks-agent-configure)），每次安装整份重写。

```
# gpuworks agent configuration, written by `gpuworks-agent configure`.
SERVER=https://gpuworks.example.com:8800
TOKEN=<节点 token>
CONNECT_MODE=agent_dial
LISTEN=
RUNTIME_DRIVER=
MOUNT_ROOT=
UDOCKER_BUNDLE=
```

- 每行 `KEY=value`，值按字面读取，没有引号、转义和变量展开；`#` 开头的行与空行跳过；不认识的键被忽略，所以新版安装脚本写的文件不会挡住旧 agent 启动。
- `SERVER` 只有 `agent_dial` 的节点有；`server_dial` 的节点留空（它从不连接 server）。`LISTEN` 只对 `server_dial` 有意义，默认 `0.0.0.0:18800`。
- 所有启动方式都只执行 `gpuworks-agent run --state <state>`：

| 启动方式 | 命令 |
|---|---|
| systemd 单元 `gpuworks-agent.service` | `ExecStart=<dir>/gpuworks-agent run --state <state>`；单元其余内容（`ExecStartPre`、`KillMode=process`、`Restart=always`、`RestartSec=3`）与配置无关 |
| nohup | `nohup <dir>/gpuworks-agent run --state <state>`；`<state>/agent-restart.sh`（`0700`）是同一条命令的重启脚本，PID 在 `<state>/gpuworks-agent.pid` |
| launchd（macOS） | `ProgramArguments` 为 `<dir>/gpuworks-agent run --state <state>`，plist 权限 0600 |
| 手工 `gpuworks-agent upgrade` 后的重启 | `run --state <state>`；没有配置文件的老 agent 才带完整参数 |

- token 因此不出现在 `ps`、`systemctl cat gpuworks-agent`、plist 或 `agent-restart.sh` 里。
- 要改配置就重新执行安装（或 `configure`）再重启 agent。临时覆盖用命令行 flag 或环境变量（优先级见 [`run`](cli.md#gpuworks-agent-run)）。
- 文件不存在不是错误：较早安装的 agent 没有它，仍从自己的命令行参数运行，升级二进制不会改动它们的启动方式；重新执行一次安装脚本后才迁移到文件。
- `gpuworks-agent uninstall` 同时删除这份文件。
- 安装脚本、SSH 向导的预检查和带外恢复要知道主机上的 agent 属于哪个 server 或哪个 token 时，读的就是这个文件（找不到文件时才读老 agent 的命令行）。设计依据见 [ADR 0051](../adr/0051-agent-launch-configuration-file.md)。
