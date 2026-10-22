# 给控制台一个公网地址

**适用场景**：server 跑在 NAT 后面（开发机、GPU 节点、内网服务器），想临时给别人一个公网地址来访问控制台。

**前置条件**：

- server 至少启动过一次（`gpuworks serve` 或 `gpuworks install` 装好的服务），它会记下自己实际监听的地址，见[安装和升级控制面](install-server.md)。
- server 主机能访问公网（下载 cloudflared 并连接 Cloudflare）；quick 提供方不需要 Cloudflare 账号和域名。
- 只作临时入口：quick 地址在连接器重建时会变（server 重启不会重建它），且不支持 SSE；节点安装请填 server 自身的稳定地址。

控制面跑在 NAT 后面（开发机、GPU 节点、内网服务器）时，用 `gpuworks expose` 把它的入口发布到
公网。一个 data-dir 只管理**一个入口**，只选择提供方（provider）。

| 提供方 | 需要准备 | 地址 | SSE（流式对话、实时日志） | 节点接入 |
|---|---|---|---|---|
| `cloudflare-quick` | 无账号、无域名 | 临时随机域名；server 重启不变，连接器重建后变化 | 不支持 | 请填 server 自身的稳定地址 |

`cloudflare-named`（固定域名 + 支持 SSE）已在代码里实现并有离线用例，但**尚未对用户开放**，原因见下文"尚未开放：named"。

## 连接器服务与 server 是分开的

cloudflared 不由 server 托管，而是跑在一个独立的**连接器服务**（`gpuworks expose run`）里：

```text
gpuworks expose run          连接器服务：读配置、下载 cloudflared、失败重试、探测、写状态
 └─ gpuworks expose connector-host   helper：持有 cloudflared，服务一退出就带走它
     └─ cloudflared tunnel --url <server 的监听地址>
```

- **server 重启、升级都不碰它**，quick 地址保持不变。server 停着的那段时间，地址返回 Cloudflare 的错误页，
  入口状态显示 `degraded`；server 回来后自动恢复为 `ready`。
- server 每次启动都会把自己实际监听的地址写到 `<data>/expose/server-origin`，`expose up` 把它作为转发目标
  写进配置；也可以用 `--origin http://HOST:PORT` 指定。
- **默认手动启动**：`gpuworks expose up` 把连接器服务作为后台进程启动（独立 session，不在
  `gpuworks.service` 的 cgroup 里），机器重启后**不会**自动恢复，需要再执行一次 `expose up`。
- **`--systemd`**：`sudo gpuworks expose up --systemd` 改为安装独立的 `gpuworks-expose.service`
  （`Restart=on-failure`、开机自启）。
- 服务以数据目录属主的账号运行（安装版里就是 server 的服务账号），root 执行 `expose up` 也不会让
  cloudflared 跑成 root。
- 入口被关闭（`expose down`）后，服务停掉 cloudflared 并自行退出；`--systemd` 模式下 `down` 还会删除 unit。

## 最短路径：quick

```bash
gpuworks expose up
```

它启动连接器服务；服务会自动找到（必要时下载并校验）cloudflared、启动连接器，`up` 等到**注册成功且
公网探测通过**后打印地址。它不会问端口、协议、IP 版本、等待时长或安装目录——这些都由实现自己决定。

**quick 的限制**（每次 ready/`status`/`--url-only` 都会提示）：地址是临时的，连接器重建后
会变（server 重启不会重建连接器，连接器服务重启会）；Cloudflare 不支持 SSE，因此流式对话和实时日志不保证逐条送达；无可用性承诺。把它当
"临时给人看一眼"的入口，不要写进长期配置或节点安装命令。

## 尚未开放：named

固定域名（`cloudflare-named`）的实现与离线用例都在，但先不给用户用。原因是它的**首次使用
门槛不在这个命令里**：tunnel token 是**运行凭据**，只能启动这一条 tunnel，不能创建 tunnel、
域名或路由；要有固定地址，你得先在 Cloudflare 控制台建 tunnel、并为一个已接入 Cloudflare 的
域名发布一条指向本 server 的路由。等这一步能收成一条有文档的简单操作（或由账户级凭据自动
完成），它再作为第二个提供方出现在 `--provider` 里。

现在的替代做法：节点请填 server 自身的稳定地址（内网 IP 或域名），不要用 quick 地址。

## 常用命令

```bash
gpuworks expose up --systemd        # 以 systemd unit 运行连接器服务（开机自启，需 root）
gpuworks expose status              # 提供方、服务方式、状态、地址、origin、连接器版本、最后错误
gpuworks expose status --url-only   # 只把地址写到 stdout（没有可用地址时退出码非零）
gpuworks expose logs -f             # 连接器日志（连接器服务轮转的那一份）
gpuworks expose down                # 关闭入口：停掉 cloudflared 和连接器服务（--systemd 时删 unit），保留配置
gpuworks expose rm                  # 关闭并删除本地配置、观测、隔离目录与托管 token
```

- `status` 只在 **连接器服务在运行 + revision 匹配 + ready** 时才给地址；锁文件存在不算在线，
  陈旧的 ready 快照也不会被当成可用地址。
- `up` 的等待有上限。超时或 Ctrl-C **只结束等待，不撤销请求**：连接器仍在连接器服务管理下，
  用 `status` 看进展，用 `down` 才停。要等更久就再跑一次 `status`。
- `down` 先写 disabled，再等连接器服务退出；`rm` 保留二进制缓存与日志（排查用），不会碰 Cloudflare
  上的 tunnel、路由与 DNS——那些是你的资源，`rm` 也不删除 `--token-file` 指向的文件。
- 配置未变时 `up` 复用当前入口（quick 不会白白换一个新地址）。

## data-dir 与 origin

- data-dir 解析顺序：`--data` > 二进制旁边的 `server.yaml` > 默认安装目录的 `server.yaml`
  （`/opt/gpuworks/server/server.yaml`）> 开发态 `./data`。CLI 每次都会打印它用了哪个
  （`--url-only` 除外，那时 stdout 只有地址）。
- origin 一般不用填：用 server 启动时记下的**实际监听**地址（通配地址转成 loopback，IPv6 也处理），
  没有记录时取安装目录 `server.yaml` 的第一个 console 地址；都不合适时用 `--origin`。server 改了监听
  地址后，重新执行一次 `expose up`。
- 入口启用期间，server 信任来自本机的连接转发的 `X-Forwarded-*`（连接器总在本机，不管 origin
  写的是 loopback 还是本机网卡地址），不需要为此改 `trusted_proxies`；`expose down` 后恢复只信任
  `trusted_proxies`，见 [ADR 0049](../adr/0049-expose-trusts-same-host-connector.md)。

## 网络与包源

cloudflared 优先用 server PATH 上兼容的版本，其次用受控缓存，最后从 `pkg.cloudflare.com`
下载（deb 走 Debian 索引的 SHA256，rpm 走 repomirror 的元数据校验；纯 Go 解包，不装系统包、
不碰 `/usr/bin`）。GitHub releases 在部分网络下不可达，所以不作为获取路径。

如果这个网络的 DNS 对新建的 quick 子域有传播延迟，`up` 会在窗口内重试探测；仍不通则把入口
标成 `degraded` 而**不是**失败——连接器保持在线，探测恢复后自动回到 ready。这样不会因为一次
DNS 抖动就换掉一个已经在用的地址。

出口、包版本与当时的速率测量（2026-09-22，上海电信，含 320 KB/s 级别的 quick 吞吐）只代表
那次测量，不是对其他网络或其他提供方的承诺。

## 安全边界

入口暴露的是整个 server：UI/API 仍需登录会话，`/v1` 仍需 API key，和在内网一样。**没有**做
路径过滤、Cloudflare Access 或登录限流——需要这些请在 Cloudflare 侧配置。团队里加节点时，
如果填的是 quick 地址，界面会提示这是临时地址。
