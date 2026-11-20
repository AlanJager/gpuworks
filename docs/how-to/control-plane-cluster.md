# 安装三成员控制面集群

本文说明怎样把控制面装成三个成员的集群：一个成员停机时其余两个继续提供管理 API、控制台和网关。行为与边界见[控制面集群的行为与边界](../explanation/control-plane-cluster.md)。

**适用场景**：控制面所在机器停机不能中断管理面；有三台 Linux 主机和一个稳定入口可用。单台控制面的安装见[安装和升级控制面](install-server.md)。

**前置条件**：

- 三台 Linux amd64 主机，各有一个互相可达的 IPv4 地址；回环地址不被接受。
- 一个稳定入口，二选一：已有的高可用负载均衡，或 keepalived VIP。不要同时使用两者。入口把请求转到三个成员的 HTTP 端口，并且必须原样透传 `/api/v1/agent/connect` 的 WebSocket Upgrade：不缓冲，不重试。agent 和用户只访问这个入口，成员地址不是受支持的入口。
- 管理机到三台主机的 SSH（免密，`BatchMode`），并且三台主机的主机密钥已在管理机的 `known_hosts` 里；安装使用 `StrictHostKeyChecking=yes`。
- 安装成 systemd 服务时，SSH 用户能免密 `sudo`，主机使用 systemd。
- 三台主机上以下端口空闲并互通：HTTP `8800`、注册 `9443`、数据库 `9001`、成员会话 `9002`（数据库端口加一）。

## 1. 构建两个制品

默认构建不含集群运行时，行为不变。集群需要带 `dqlite` 标签的 server 和数据库辅助程序 `gpuworks-dqlite`；只有后者使用 cgo。

```bash
make dqlite-deps                # 固定版本的静态原生库和打过补丁的 go-dqlite，装进 tmp/dqlite/
make dqlite-helper              # bin/gpuworks-dqlite-linux-amd64（静态链接，cgo）
make server-linux-amd64-dqlite  # bin/gpuworks-linux-amd64-dqlite（纯 Go）
```

`make dqlite-deps` 需要 `musl-gcc`，不安装任何系统包，也不下载 Go 工具链；细节见仓库里的 `scripts/dqlite/README.md`。修改 `go.mod` 之后要重新运行它，再构建两个制品。

## 2. 写 cluster.yaml

```yaml
version: 1
defaults:
  dir: /opt/gpuworks/server
  ssh:
    user: ops
    key: /home/ops/.ssh/id_ed25519
ports:
  http: 8800
  enrollment: 9443
  database: 9001
nodes:
  - name: a
    address: 10.0.0.11
    ssh: {target: 10.0.0.11}
  - name: b
    address: 10.0.0.12
    ssh: {target: 10.0.0.12}
  - name: c
    address: 10.0.0.13
    ssh: {target: 10.0.0.13}
```

- `version` 必须是 `1`；`nodes` 必须恰好三个，`address` 是互不相同的 IPv4。
- 每个节点可以用自己的 `dir` 和 `ssh`（`target`、`user`、`key`、`port`）覆盖 `defaults`。
- 安装目录在三台主机上都必须不存在；安装不会覆盖已有目录。
- 也可以运行 `gpuworks cluster install --configure`，按提问生成这个文件而不做任何远端操作。
- 可选的顶层键 `storage_tier: standard` 或 `storage_tier: slow-storage` 指定[存储档位](#存储档位)；不写时由安装按测量结果选择。命令行的 `--storage-tier` 优先于这个键。

## 3. 预检查

```bash
gpuworks cluster check --config cluster.yaml --binary bin/gpuworks-linux-amd64-dqlite
```

预检查经 SSH 在三台主机上验证目录、端口和节点间路径（注册、数据库、成员会话），结束时清理临时程序和监听端口。它不使用 `--password-file`。

网络检查通过后，预检查还会在每个节点将存放数据的文件系统上测量 30 秒的同步写延迟（4 KiB 写入后 `fdatasync`，每秒不超过 40 次，测量文件用完即删），并按最慢的节点给出建议的[存储档位](#存储档位)。`--storage-probe 0` 跳过，`--storage-probe 2m` 延长，最长 5 分钟。预检查里的档位只是建议：它不影响预检查是否通过，没有测量到的节点显示为未测量。

## 4. 安装

```bash
gpuworks cluster install --config cluster.yaml \
  --binary bin/gpuworks-linux-amd64-dqlite \
  --database-helper bin/gpuworks-dqlite-linux-amd64 \
  --password-file admin-password
```

- `--password-file` 是初始管理员口令文件：权限 `600` 的普通文件，一行，至少 8 个字节。口令不能写在命令行上；在终端里省略该参数时会提示输入。
- `--no-service` 不写 systemd 单元，以后台进程运行，主机重启后不会自动拉起，只用于预览。
- 安装依次做：预检查、分发两个制品、初始化首节点、加入其余两个节点、启动控制面、等待集群同步、业务验证。任何一步失败都会停止，远端已创建的数据和日志保留，错误写在对应安装目录的 `logs/install-node-errors.log`。
- 首次启动要在新数据库上执行全部迁移并写入内置目录，明显慢于之后的启动；存储慢的主机上更久。总时限由 `--timeout` 控制，默认 15 分钟。
- 安装的预检查同样测量存储并据此确定存储档位，见下一节。`--storage-probe 0` 跳过测量。

### 存储档位

集群的每次提交都要等多数成员完成一次同步写。数据库在一段时间联系不上 leader 后重新选举，这段时间由存储档位决定，三个成员必须相同。档位在安装时确定，由首节点记录，其余成员加入时从集群取得。

| 档位 | follower 发起选举前的等待 | 适用存储 | 杀掉 leader 进程后的恢复目标 |
|---|---|---|---|
| `standard` | 150–300 ms | 同步写不出现 150 ms 以上停顿 | 10 秒 |
| `slow-storage` | 0.75–1.5 s | 同步写有数百毫秒到数秒的停顿（如共享的网络存储） | 6 秒 |

安装按测量结果选择档位，并在进度输出里写明依据：

| 测量结果（最慢节点） | 未指定 `--storage-tier` | 指定了 `--storage-tier` |
|---|---|---|
| p99 不超过 100 ms，且没有 150 ms 以上的停顿 | `standard` | 使用指定档位 |
| p99 超过 100 ms，或出现过 150 ms 以上的停顿 | `slow-storage` | 使用指定档位；指定 `standard` 时给出警告 |
| p50 超过 150 ms，或最大值超过 5 s | 在任何远端写入之前停止 | 使用指定档位并给出警告 |
| 未测量（跳过，或节点上无法创建测量文件） | `standard`，并提示 | 使用指定档位 |

只看 p99 不够：实测过一台 p99 为 18.5 ms 的主机，30 秒里有一次 2.25 秒的同步写，一次这样的停顿就可能让 leader 失去角色。所以档位同时看最大值和 150 ms 以上停顿的次数。

恢复目标来自在 4 核、共享网络存储（`fdatasync` p50 约 4 ms、p90 20–80 ms、p99 0.5–1.9 s）的三台成员上的实测，每个档位杀 leader 5 次：`standard` 恢复 0.78–2.11 秒，`slow-storage` 恢复 1.71–3.07 秒。样本很小，说明不了尾部；它们不是对其他存储的保证。在同样的存储上：

- `standard` 档位下集群空闲时约每 5–6 分钟自行切换一次数据库主节点；`slow-storage` 下 27 分钟里一次。`slow-storage` 减少这种切换，不能消除。
- 三台成员的存储同时停顿数秒时，两个档位都会有请求失败。更长的选举等待对此没有帮助。
- 慢速存储上首次启动要几分钟。

更换已安装集群的档位没有命令。它需要三个成员同时停止，把每个成员 `<数据目录>/cluster-enrollment/config.json` 里的 `storage_tier` 改成同一个值（`standard` 是删去该键），再一起启动；成员之间档位不一致时选举时间不一致。`gpuworks cluster-status` 显示本成员记录的档位。

每个成员的安装目录里有：

| 路径 | 内容 |
|---|---|
| `<dir>/gpuworks` | server |
| `<dir>/gpuworks-dqlite` | 数据库辅助程序，由 server 启动，随 server 退出 |
| `<dir>/server.yaml` | 监听地址、数据目录和日志；不含凭据 |
| `<dir>/data` | 数据目录，含成员证书与数据库 |
| `<dir>/logs/server.log` | server 日志；systemd 单元的标准错误同时进入 journal |

systemd 单元名是 `gpuworks-cluster-<安装目录绝对路径 SHA-256 的前 16 个十六进制字符>.service`。

## 5. 确认就绪

对每个成员的 HTTP 端口：

```bash
curl -s http://10.0.0.11:8800/api/v1/readyz
```

就绪的成员返回 200，正文恰好是 `{"status":"ready"}`。还没有开放业务的成员返回 503，正文是 `member business service is not ready`。负载均衡或 keepalived 的健康检查应当用这个路径，并且只把返回 200 的成员放进转发池。

在某个成员上查看它自己观察到的集群状态：

```bash
gpuworks cluster-status --data /opt/gpuworks/server/data
```

输出包含成员角色、投票成员数、是否容错、各监听器状态、业务是否开放以及当前 leader。

## 成员侧命令

`cluster install` 在每台主机上调用下面这些命令；手工修复时才直接使用。它们只存在于带 `dqlite` 标签的 Linux 构建里，参数见 [CLI 参考](../reference/cli.md#gpuworks-cluster)。

- `gpuworks cluster-init`：在已停止的首节点上生成集群身份和注册监听配置。
- `gpuworks cluster-address`：在已停止的首节点上把数据库地址改为成员证书对应的地址。
- `gpuworks cluster-join`：凭邀请令牌把一个新成员加入集群。
- `gpuworks cluster-status`：只读地报告本成员看到的状态。
