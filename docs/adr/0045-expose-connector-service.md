# ADR 0045: 公网入口的连接器跑在独立的连接器服务里，不随 server 启停

日期：2026-10-14 · 状态：已接受；修改[公网入口计划](../../artifacts/archive/2026-09-22-public-expose/plan.md)中「连接器由 server 托管、与 server 同生共死」的取舍

## 背景

`gpuworks expose` 原来由 server 托管 cloudflared：helper 监视 server 持有的管道，server 一退出（包括 SIGKILL）就带走连接器；systemd 的 control-group 也会在 server 重启时杀掉它。当时的理由是"入口指向一个已经停掉的 server，比入口需要重建更糟"。

实际使用中这个代价太高：quick tunnel 的地址按连接器分配，server 每次重启或升级都会换一个新地址，已经发出去的链接随之失效。而 server 停着时，旧地址返回的只是 Cloudflare 的错误页，并不会把流量交给别人。

## 决策

1. **独立的连接器服务**：`gpuworks expose run` 运行原来的 supervisor（下载 cloudflared、网络重试、探测、状态），负责托管 cloudflared。helper 的 liveness 管道改为绑定这个服务，服务退出（包括被 SIGKILL）时连接器跟着退出。server 不再托管入口，只在每次启动时把实际监听地址写到 `<data>/expose/server-origin`。
2. **默认手动启动**：`expose up` 把服务作为后台进程启动（独立 session，不在 `gpuworks.service` 的 cgroup 里），机器重启后不自动恢复。`expose up --systemd` 则安装独立的 `gpuworks-expose.service`（`Restart=on-failure`、开机自启）。`gpuworks.service` 本身不做任何修改。
3. **转发目标写进配置**：`up` 按 `--origin` > server 记录的地址 > 安装目录 `server.yaml` 的顺序确定 origin，写进 `config.json`。origin 变化算一次新请求。
4. **server 不在不算失败**：本地 origin 探测不通时，入口标为 `degraded` 并保留连接器，server 回来后恢复为 `ready`。
5. **服务随入口关闭退出**：入口被 disable 后，服务停掉连接器并以 0 退出（所以 unit 用 `Restart=on-failure`）。`expose down` 等服务退出，`--systemd` 时还会 `disable --now` 并删除 unit。
6. **运行账号**：服务以数据目录属主的账号运行。root 执行 `up` 时降权到该账号，systemd unit 写 `User=`。

## 取舍

- server 停着时地址仍然解析，只是返回错误页。换来的是 server 重启和升级后地址不变。
- 默认的后台进程在机器重启后不会恢复，需要手动再执行一次 `up`；要自启就显式选择 `--systemd`。这样不改动 server 的 unit，也不会在用户没要求时多出一个开机服务。
- 一个数据目录只能有一个连接器服务（沿用 supervisor 锁）。连接器服务本身重启时会重建连接器，quick 地址随之变化。
