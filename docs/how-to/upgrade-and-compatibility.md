# 升级与版本兼容

**适用场景**：要把已在运行的集群升级到新版本，想提前知道升级会不会影响正在服务的实例、该先升 server 还是先升 agent、server 与 agent 版本不一致时会发生什么，以及出问题怎么回退。

**前置条件**：

- server 已按[安装和升级控制面](install-server.md)用 `gpuworks install` 装好（`gpuworks upgrade` 只认有 `server.yaml` 的安装目录）。
- 节点已按[接入、升级和卸载节点](connect-nodes.md)接入；升级 agent 需要管理员账号。

## 先看结论

- **先升 server，再升 agent。** 节点页「升级 Agent」让 agent 从它当前连接的 server 的 `/dl/` 下载二进制，而 `/dl/` 里的 agent 是 server 构建时内嵌的、与 server 同一个版本号。没有先升 server，点「升级 Agent」只会下载到旧版本。
- **升级 server 不会升级任何已接入的 agent**，也不会停掉节点上的推理容器；节点 agent 断线后自动重连。
- **server 与 agent 之间没有强制的最低版本检查。** 握手只协商协议号区间（当前两端都是 1–1，不会不匹配），不比较发行版本号；老 agent 缺少的能力，由 server 在用到那项能力时逐项降级或拒绝。详见下文[版本不一致时会怎样](#版本不一致时会怎样)。
- **数据库迁移只向前**，没有 down 迁移；回滚靠升级前自动生成的备份，不靠反向迁移。

## 升级前检查

1. 看有没有新版，不改动任何东西：

   ```bash
   gpuworks upgrade --check
   ```

   本地构建（版本号为 `dev` 或带 `-dirty`）没有 release 标识，不加 `--force` 会被拒绝。
2. 确认安装目录所在磁盘有空间：备份是 `<dir>/backups/` 下的 tar.gz，含整个 `gpuworks.db`（连同 `-wal` / `-shm`）、`master.key`、`server.yaml` 和当前二进制；默认保留最近 3 份，可用 `--keep N` 调整。`logs.db`、`metrics.db`、`ux.db`（逐请求记录）、镜像与目录包缓存**不在**备份里。
3. 选低峰期。server 重启会断开全部在途的流，已经开始的响应不会重试（见[生产环境部署](deploy-production.md)）；经 server 发压的评测（`load_origin` 为 server）在重启后会被标记为失败，从 agent 发压的评测不受此规则影响。
4. 避开长动作：模型下载、节点间同步、镜像同步尽量等结束再升级。镜像的节点间传输在 server 或节点重连后会重新协调，Docker 归档从第 0 字节重来。
5. 多机推理或 PD 分离部署的 agent 一次只升一个节点（原因见[升级 agent](#升级-agent)）。
6. 记下当前版本：`gpuworks version`，或 `GET /api/v1/healthz` 返回的 `version`。

## 升级 server

```bash
gpuworks upgrade
```

流程固定为：停服 → 打包备份 → 原子替换二进制 → 重启 → 探活（60 秒）。任何一步失败：

- 停服之前的失败（下载、校验、取锁）：不改动任何东西。
- 停服后备份失败：原版本被重新拉起，不做任何替换。
- 探活失败或新二进制起不来：自动还原备份、重启旧版本，命令以非零码退出并打印备份路径。

参数：

| 参数 | 用途 |
|---|---|
| `--check` | 只报告是否有新版，不改动任何东西 |
| `--force` | 允许替换没有 release id 的本地构建（`dev` / `-dirty`） |
| `--keep N` | 保留的备份份数，默认 3 |
| `--version ID` | 期望的 release id；与发布站当前发布（或发布包里的版本）不一致时报错，不会静默升到别的版本 |
| `--base-url URL` | 内网镜像站 |
| `--archive FILE` | 本地发布包，主机访问不到发布站时用，见[离线升级](install-server.md#离线升级) |

全部参数见[CLI 与安装脚本](../reference/cli.md)。并发升级由 `<dir>/.upgrade.lock` 互斥。每次升级追加一行审计到 `<data>/install-history.jsonl`（动作、结果、新旧 release、备份路径，失败时含错误）。

### 数据库迁移

迁移随 server 启动自动执行，不需要单独步骤：`serve` 打开 SQLite 时按版本号顺序执行尚未应用的迁移，已应用的版本记录在 `schema_migrations` 表里，下次启动直接跳过。

- 每个迁移和它的版本标记在同一个事务里提交；某个迁移出错则该迁移整体回滚，server 带着 `migration N (name): …` 错误退出、不会起来。重启会从失败的那个版本重试。
- 没有 down 迁移。新版本迁移过的库交给旧二进制，代码不会检查「库比二进制新」，旧二进制只会跳过它不认识的版本；因此**不要**单独换回旧二进制而保留新库，回退一律连同备份里的 `data/` 一起还原。
- 逐请求记录 `ux.db` 有自己的 schema 版本，同样在启动时自动迁移、与版本号同一事务提交。它**会**检查「库比二进制新」：旧二进制遇到新版本迁移过的 `ux.db` 拒绝启动（`uxstore: file schema version N is newer than M`）。`ux.db` 不在备份里，所以回退到 `ux.db` 迁移之前的版本（包括升级失败后的自动回滚）时，要先把 `data/ux.db`、`ux.db-wal`、`ux.db-shm` 挪走；它只是请求观测数据，旧版本会新建一个空的。版本 2 起（网关耗时与引擎预处理分开记录）无法降级到更早的版本而保留这份数据。
- `gpuworks migrate [--data DIR] [--db PATH]` 对指定 SQLite 运行同一套迁移与内置目录 seed，并打印当前版本。它会写库，想预演时对备份或拷贝出来的库运行，不要对正在被 server 使用的库运行。

### 回滚

自动回滚只覆盖升级命令自己发现的失败（二进制起不来、探活超时）。新版本起来了但你发现行为不对，需要手工回退到升级前快照：

1. 停服务：`sudo systemctl stop gpuworks`（nohup 模式按 `gpuworks.pid` 结束进程）。
2. 备份文件名是 `<被替换的 release id>-<UTC时间戳>.tar.gz`，在 `<dir>/backups/`。解到临时目录，成员布局为 `data/gpuworks.db[-wal|-shm]`、`data/master.key`、`server.yaml`、`bin/gpuworks`。
3. **先删掉当前的 `data/gpuworks.db-wal` 与 `data/gpuworks.db-shm`**，再把 `data/*` 拷回数据目录、`bin/gpuworks` 拷回 `<dir>/gpuworks`、`server.yaml` 拷回 `<dir>`。先删 WAL 是为了避免新版本写下的前滚日志被旧 schema 重放。
4. 启动服务并检查 `/healthz`。

回滚还原的是升级前快照，升级窗口（约 1 分钟）之后写入的数据会丢。

## 升级 agent

两条路径，效果相同：

- **节点页「升级 Agent」**（`POST /api/v1/nodes/{uuid}/upgrade`）：节点须在线，接口立即返回 `upgrading`，仅支持 Linux。按 agent 版本有两种做法：
  - agent 握手时声明了 `upgrade.push` 能力：server 按节点上报的架构取 `gpuworks-agent-linux-<arch>`，经**节点已有的连接**推过去（附长度与 sha256）。agent 写入 `<bin>.new`、校验长度与 sha256、运行一次 `<bin>.new version` 确认能执行，应答后旧二进制改名 `<bin>.old`、换上新版、原地 `exec`。不需要节点访问 server 的地址，所以 Server 发起连接（`server_dial`）的节点也能这样升级。推送有 10 分钟预算。
  - 更早的 agent：server 通过 `agent.upgrade` RPC 通知，agent 从 `<server>/dl/gpuworks-agent-linux-<arch>` 下载到 `<bin>.new`，用新二进制对当前 server 跑一次 `smoke`（10 秒，只校验认证与传输）后同样替换并 `exec`。这条路要求节点能访问 server 的地址。
- **重新运行 `/install.sh`**（Server 发起连接的节点用[离线包](connect-nodes.md#离线包)）：直接替换二进制并 `systemctl restart`，不做 smoke 校验，同时按本次参数重写 `<state>/gpuworks-agent.conf`，较早安装的 agent 借此迁移到配置文件。适合节点已离线、或 agent 版本过旧没有自升级能力的情况。

结果怎么判断：

- 新版本重连后，节点页「Agent」列的版本号变成 server 的版本号；升级失败时 agent 上报 `agent.upgrade.status`，该列显示红色「升级失败」，悬停看原因。下载 404、smoke 失败、server 不是内嵌 agent 的开发构建都属于这一类，此时旧二进制保持不动。
- 页面自己只等 2 分钟，超时提示「升级未在 2 分钟内完成，请检查 Agent 日志」；agent 侧下载预算是 10 分钟，慢链路上页面超时不代表升级失败，以节点页版本号与 `agent.log` 为准。
- **启动后自动回滚**：替换时 agent 在 `<bin>.upgrade-pending` 记一个 6 分钟的确认期限（覆盖 server_dial 最长 5 分钟的重拨间隔）。新进程只有和 server 完成一次注册握手才算确认并删掉这个文件；期限内没被连上，或新版本连续启动 3 次都没走到注册（例如启动即崩、被 systemd 反复拉起），就把 `<bin>.old` 放回原位并以原参数 `exec` 旧版本，旧版本连上后上报 `agent.upgrade.status`，节点页显示「升级失败」，原因带 `rolled back:`。server 暂时不可达也会触发回滚，此时旧版本照常工作，server 恢复后重新点升级即可。
- 没有「新版连上了但行为不对」的自动回滚：确认之后旧二进制仍留在 `<bin>.old`，需要时停止 agent、改名放回、再启动。

**运行中的实例**：自升级是同一进程 `exec`，SSH 或 `install.sh` 安装的 systemd 单元设了 `KillMode=process`，`install.sh` 重启 agent 也不会杀推理容器。agent 重连后 server 发 `state.sync`（全量期望状态），agent 按容器的确定名（`gpuworks-<uuid 前 12 位>`）检查：容器还在运行就**接管**（不重新拉镜像、不重新启动，也不重复 sleep/wake 预热；可休眠实例还会探测 sleep 状态）；容器存在但已停止的先删除再新建，不存在的直接新建。runveil 节点用 pid 文件和进程存活判断，同样接管。孤儿容器清理默认关闭，是设置页可选开启的项目，升级不会误删。

但经该节点隧道转发的流量在 agent 断开期间不可达：断线时该节点上的在途流会被断开，新请求要等重连完成，agent 重连用 1 秒起步、最长 30 秒的退避。多机推理的某个 rank 所在节点离线超过 2 分钟（宽限期）会使这一代（generation）整体失败，所以多机或 PD 分离的节点逐个升级、每升一个等节点回到在线再动下一个。

推荐顺序：

1. 升级 server，确认节点页恢复在线、`GET /api/v1/healthz` 的版本号是新版本。
2. 在节点页按「可升级」标签逐个升级 agent；先挑没有实例或可中断的节点验证。
3. 全部完成后，节点页「Agent」列不再出现橙色「可升级」标签。

注意：手工命令 `gpuworks-agent upgrade` 替换完成后会 `exec` 成前台运行的 `run`（按代码推断，未实机验证），不会替你重启 systemd 服务；常驻 agent 已经在运行时，会多出一个使用同一 node token 的进程，而 server 对同一节点只保留最新会话。受 systemd 托管的节点请用节点页按钮或 `install.sh`，手工命令留给没有常驻 agent 的环境。通过 RPC 触发的升级会原样保留启动参数；手工命令在配置记在 `<state>/gpuworks-agent.conf` 里时（较新的安装）只带 `--state`，较早安装、没有这份文件的 agent 则带 `--server`、`--token`、`--connect-mode`、`--state`（`server_dial` 再带 `--listen`）与可选的 `--driver`；都不带 `--data-lane` 与 `--dcgm`。Server 发起连接的 agent 没有 server 地址，手工 `upgrade` 直接拒绝，只能用节点页按钮（server 推送）。

## 版本不一致时会怎样

以下全部来自握手与各功能点的代码，而不是发布承诺。

### 握手

- agent 在 `hello` 里带上 `proto_min`/`proto_max`、自己的 `version` 和能力列表 `caps`（例如 `webterm`、`bench`、`gpu.nvidia-smi`）。server 取两端协议区间的交集；交集为空就回 `reject{reason: proto_mismatch}` 并在节点上记下拒绝原因，agent 隔 5 分钟重试。当前所有构建的区间都是 1–1，所以**现在不会发生** proto_mismatch，这是为将来预留的机制。
- server 只**记录**agent 的 `version`（`agent_version`）并据此做下面列出的个别判断；server 回给 agent 的 `hello` 里有自己的版本号，但 agent 不读取、不比较。**agent 侧没有「server 太新/太旧」检查。**
- 任何一端收到自己不认识的消息类型，都是忽略并记一条 debug 日志，不会断连。

### 双通道（lane）

- 新 agent 默认用 `?lane=ctrl` 连接并期待 `Registered.session_id`，有 session_id 才再开一条数据通道；连到不发 `session_id` 的老 server 时，agent 静默保持单通道，不会报错。
- 老 agent 不带 `lane` 参数连新 server，server 按单通道处理（所有流都走这一条连接）。`--data-lane=false` 与 `server_dial` 同理。
- 两种组合都能工作，差别只在吞吐与延迟隔离；数据通道掉线时绝不回退到控制通道，见 [ADR 0024](../adr/0024-data-lane-no-ctrl-fallback.md)。

### 新 server + 老 agent：逐项降级

server 用到某项 agent 能力时才检查，检查方式有四种：

| 功能 | 怎么判断 | 老 agent 上的表现 |
|---|---|---|
| Web 终端 | agent 是否在 `hello.caps` 里声明 `webterm`；容器终端另看 `webterm.container`（runveil/udocker 节点看 `webterm.container.udocker`） | 宿主机终端被拒绝：「this node's agent does not support terminals; upgrade the agent」；容器终端被拒绝：「this node's agent does not support container terminals」（runveil 节点为「runveil container shell is unavailable on this agent」） |
| 镜像拉取（`image.pull`）、从 URL 加载镜像（`image.load_url`） | 调用 RPC，agent 回 `unknown rpc method` | 返回产品错误 `AGENT_UPGRADE_REQUIRED`（界面：「目标节点的 agent 版本过旧；请先升级 agent，再拉取镜像」）或对应的镜像行置为失败并提示升级 |
| 从 HTTP 目录下载模型 | 节点记录的 `agent_version` 能解析为 `主.次.修` 且小于 `1.0.0` | 下载被拒绝，返回 `MODEL_SOURCE_REQUIRES_NEWER_AGENT`；版本号解析不出来（如 `dev`、裸 commit）则放行，由 agent 自己报错 |
| 评测的 tokenizer 探测（`fs.stat`） | 调用 RPC | 老 agent 答不出时当作路径存在继续，最后仍走估算模式并给出 tokenizer 警告 |
| dGPU 能力 | 节点上报的 dGPU 安全协议号（`partition_safety_protocol`）或分区探测结果是否存在 | 该节点的 dGPU 能力显示为不可用，原因「agent has not reported current dGPU safety capability; upgrade the agent」 |

以上是 server 代码里明确针对老 agent 做的判断。其余新增字段（例如网络清单）在老 agent 上缺失时，server 按「字段缺失」处理；具体到某个页面缺什么，以当时的界面为准，没有逐项列表。

### 老 server + 新 agent

没有检查，也没有承诺：agent 不会拒绝老 server；新 agent 发出的、老 server 不认识的消息被忽略，新 agent 期待的 server 行为（例如新的 RPC 或期望状态字段）老 server 不会提供。不要长期保持这种组合；遇到这种情况的正确做法是把 server 升上去。

### 节点页怎么显示

- 「Agent」列显示 `agent_version`（没有握手过则显示「—」），详情抽屉里是「Agent 版本」；API 对应 `GET /api/v1/nodes` 里每个节点的 `agent_version`（握手过才有），`GET /api/v1/healthz` 的 `version` 是 server 版本。
- 橙色「可升级」标签的判断是纯字符串不相等：server 与 agent 版本号都不是 `dev`，且二者不同。它**不比较新旧**：比 server 还新的 agent 也会显示「可升级」，此时点「升级 Agent」会下载 server 内嵌的（更旧的）版本，相当于降级。`dev` 构建不显示标签。
- 升级中显示「升级中」，失败显示「升级失败」。失败记录在 server 内存里（`GET /api/v1/agent/upgrade-errors`），agent 之后用与 server 相同的版本号重连时清除；server 重启会丢掉这份记录。
- 握手被拒绝时，协议不匹配的原因会记在对应节点上，显示在节点详情的「最近拒绝」里；token 无效这类拒绝不属于任何节点，节点页不显示，见 server 日志或 `GET /api/v1/agent/rejects`。

## 各动作的影响与回滚

| 升级动作 | 对运行中实例的影响 | 回滚方式 |
|---|---|---|
| `gpuworks upgrade`（server） | 推理容器与模型不动；`/v1` 网关约 1 分钟不可用，在途流断开；server 侧发压的评测被标记失败；各节点 agent 断线后自动重连并重放 `state.sync`，接管原有容器 | 探活失败自动还原；其余情况按上文手工还原备份 |
| 数据库迁移（随 server 启动） | 无直接影响；迁移失败则 server 起不来，实例照常运行、网关不可用 | 不回滚迁移，还原升级前备份 |
| 节点页「升级 Agent」 | 容器保留，agent 重连后接管；该节点的流量短暂中断；多机推理节点离线超过 2 分钟会使整组失败 | 下载或 smoke 失败自动保留旧二进制；`exec` 失败自动放回；`exec` 之后出问题手工改回 `<bin>.old` |
| 重新运行 `/install.sh` | 与上相同，`KillMode=process` 保证容器不被杀；重启 agent 有短暂断线 | 无 smoke、无自动回滚，升级前自行备份旧二进制 |
| `gpuworks-agent upgrade`（手工） | 见上文注意事项 | 同节点页升级 |

## 相关文档

- [安装和升级控制面](install-server.md)：目录布局、备份内容与 `upgrade` 流程细节。
- [接入、升级和卸载节点](connect-nodes.md)：agent 接入与卸载。
- [CLI 与安装脚本](../reference/cli.md)：`upgrade`、`migrate`、`smoke` 的参数与退出码。
- [生产环境部署](deploy-production.md)：升级时 `/v1` 网关的行为。
- [架构总览](../explanation/architecture.md)：连接、重连与期望状态重放。
