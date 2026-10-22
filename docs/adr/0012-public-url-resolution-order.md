# ADR 0012: PublicURL Resolution Order

**Status**: Superseded by [ADR 0025](0025-server-holds-no-external-address.md)
**Date**: 2026-08-23
**Context**: M21 云接入公网地址与安装链路修复

---

## Context

安装 agent 时必须把 server 的外部地址（`PublicURL` / `server_url`）烤进启动命令。这个地址的正确性直接决定 agent 能否连上 server——错了就是全网节点永不可达。commit `51777f2` 引入了自动探测本机出口 IP 的 `inferLocalPublicURL`，在 `app.New` 启动时预填 `cfg.PublicURL`，目标是「不配 `--public-url` 也能开箱即用」。

但预填带来一个副作用：api 层四处（`api/provider.go:288`、`api/provision.go:41`、`api/api.go:326`、`app/install.go:117`）的 `if PublicURL == "" → inferRequestBaseURL(r)` 全部变成死代码——不管用户从什么地址（反代/域名）访问，装出去的 agent 一律拿到内网 IP。2026-08-23 mirror1 实测：用户经 `a2.tunnelfrp.com:8800` 反代访问 UI，但 RunPod pod 上 agent 实际是 `--server http://<内网 IP>:8800`（mirror1 的内网地址，pod 不可达），节点永远「接入中」。

关键矛盾：开箱即用（本机 IP 兜底）与反代正确性（请求 Host 优先）二者在预填策略下不可兼得。

---

## Decision

PublicURL 解析优先级重排为三级，每级严格覆盖下一级：

1. **显式配置**：`--public-url` / `GPUWORKS_PUBLIC_URL` / 请求里的 `server_url` 字段
2. **请求 Host 推断**（`inferRequestBaseURL`）：从请求的 `Host` 或 `X-Forwarded-Host`（仅在 `TrustedProxies` 配置命中时采信）+ `X-Forwarded-Proto` 构造基址；Host 为 loopback（`127.*` / `::1` / `localhost`）时跳过本级
3. **本机出口 IP 兜底**（`inferLocalPublicURL`）：仅在前两级都不可用时使用，响应中标注「推断自本机网卡，跨网段不可达」

实现：

- `app.New` 不再预填 `cfg.PublicURL`；本机 IP 作为独立的 `FallbackPublicURL` 字段传入 `Deps`
- api 层四处收敛为一个 `resolvePublicURL(r *http.Request)` helper，实现上述三级逻辑（含 trusted-gated XFH 检查）
- `/install.sh` handler 同样逻辑
- 响应回显最终使用的 `server_url`（含来源标注），供用户核对

`X-Forwarded-Host` 仅在 `TrustedProxies` 匹配时采信的理由：裸 `Host` 头是 HTTP 协议内生字段、由 client 设置，而 `X-Forwarded-*` 是代理层注入、可被中间人投毒；frp/ngrok 等 TCP 反代不改 `Host`，裸 `Host` 已覆盖常见用法；七层反代场景本来就该配 `TrustedProxies`。

---

## Consequences

### Positive

- **反代场景零配置可用**：用户经反代访问 UI，装出的 agent 自动用反代地址，无需手动填 `--public-url`
- **显式配置始终生效**：`--public-url` 不会被任何推断覆盖，行为可预测
- **开箱即用保留**：本机 `curl` / 同网段浏览器访问时，兜底仍能生成可用命令
- **安全边界清晰**：`X-Forwarded-Host` 仅在白名单代理时采信，降低头注入风险

### Negative

- **loopback Host 只能得到启发式兜底**：用户从本机或 SSH 端口转发访问（Host 为 `127.*`、`::1` 或 `localhost`）时，请求地址本身不能供远端 agent 使用，因此只能回退到本机出口网卡地址。该地址对跨网段、NAT 或反代场景未必可达，用户仍可能需要显式配置 `--public-url` 或修改安装表单里的 `server_url`
- **响应多一行标注**：本机 IP 兜底时会标注「推断自本机网卡，跨网段不可达」，稍增 UI 复杂度——但这正是预防本次事故类型错误的最便宜手段

### Neutral

- **`TrustedProxies` 空时 XFH 不采信**：默认配置下 `X-Forwarded-Host` 被忽略——这是有意为之（安全优先），七层反代用户需显式配白名单
- **单测需调整**：过去单测显式注入 `PublicURL` 恰好绕开了本次 bug，现在要补「反代 Host 访问时推断正确」的 API 级断言

---

## Alternatives Considered

### A. 删除本机 IP 兜底

loopback Host 时直接返回错误「无法确定外部地址，请配置 --public-url」。

**拒绝理由**：`make demo`、文档 quickstart、本机 curl 生成命令给同网段机器用都是真实场景，报错会伤害开箱体验；而兜底结果在响应里标注后，用户能立即看出是否适用，不会盲目使用。

### B. 始终优先请求 Host，不做兜底

删除 `inferLocalPublicURL`，loopback 或无请求上下文时返回空 / 报错。

**拒绝理由**：与 A 相同——开箱体验受损；且「安装命令生成」API 无请求上下文（可能由后台任务调用），没有兜底会让这类场景完全不可用。

### C. 预填改为「首次请求时懒填充」

`app.New` 不预填，但 api 层首次调用 `resolvePublicURL` 时写回 `cfg.PublicURL`，后续请求复用。

**拒绝理由**：首次请求可能是内网地址或 loopback，写回后会污染全局配置；且引入状态（谁先调用、并发竞争），增加复杂度。不如每次请求独立解析。

---

## Update (2026-09-10): 控制面自安装默认不写 PublicURL

`gpuworks install` / `gpuworks upgrade` 落地后，这条决策成为安装程序的默认：install 生成的
systemd 单元与 nohup 启动参数**只在用户显式传 `--public-url` 时**才写 `GPUWORKS_PUBLIC_URL`。
理由是本次事故的同一个教训——安装时预填一个地址（尤其是 `/opt` 下的服务，几乎总是装在内网）
会让后两级解析全部失效：节点安装命令从此固定指向那个地址，反代、换网段、迁机都会踩同一个
坑。默认留空后，每台机器按请求 Host 三级解析，「不配置」成为安全的那一侧。

`install.json` 记录实际传入的 PublicURL，upgrade 用它重建单元，因此升级不会悄悄打开或关闭
这一设置。相关用法见[安装和升级控制面](../how-to/install-server.md)。

---

## Update (2026-09-25): 设置页的访问入口属于"显式配置"一级

设置页新增「访问入口」（`access` 设置：管理入口、推理网关入口、添加计算节点的默认地址）。它们与
`--public-url` 同属第一级显式配置，且设置页优先于 `server.yaml` 的 `public_url`：节点安装地址取
"添加计算节点的默认地址"，没填则取"管理入口地址"，都没填才回到本 ADR 的三级解析。三项默认留空，
延续上面"不预填、不配置是安全的一侧"的结论；响应里的 `server_url_source` 仍为 `explicit`。

## Update (2026-09-26): 显式配置改由 server.yaml 承载

`GPUWORKS_PUBLIC_URL` 和 `install.json` 已不存在，单元文件不再携带任何设置。显式配置这一级
现在来自安装目录下 `server.yaml` 的 `public_url` 键，或 `serve --public-url` 命令行参数；三级
解析顺序本身不变。

---

## Related

- [M21 修复 plan](../../artifacts/archive/2026-08-20-images/plan-m21-cloud-install-fix.md)：完整事故分析与修复项
- [CONTEXT.md § server 外部地址](../../CONTEXT.md)：术语定义与 Avoid 列表
- Commit `51777f2`：引入 `inferLocalPublicURL` 预填的原始 commit
- 事故取证：mirror1 实测记录见 plan 现象节
