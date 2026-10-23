# 生产环境部署

**适用场景**：server 要面向团队或生产使用：前置反代和 TLS、承载 `/v1` 网关流量、规划容量与主机参数、保护凭证与备份、升级。

**前置条件**：

- 已按[安装和升级控制面](install-server.md)装好 server。
- 有 TLS 证书，或使用 Nginx、Caddy、云负载均衡终止 TLS，并有域名或固定入口地址。
- 对 server 主机有 root 权限，用于调整 `net.core.somaxconn`、文件描述符限制和 service 环境变量。

## 反代与 TLS

生产入口必须使用 HTTPS。可以配置下文的原生 TLS，或前置 Nginx、Caddy、云负载均衡终止 TLS，并仅在可信代理网段配置
`server.yaml` 的 `trusted_proxies`（或 `--trusted-proxies`）。只有来自这些地址的
`X-Forwarded-*` 头会被采信：`X-Forwarded-For` 用于记录 agent 接入连接的来源地址，
`X-Forwarded-Host` / `X-Forwarded-Proto` / `X-Forwarded-Port` 用于控制台写请求的 Origin 检查和
Secure Cookie，都不用于生成任何对外地址。server 不存储也不推断自己的对外
地址：给用户和 install script 用的地址来自浏览器当前访问的地址（UI 各处据此预填，可以改）或
调用方在 API 里显式传入的 `server_url`，反代或域名场景下这个地址天然就是正确的那一个，见
[ADR 0025](../adr/0025-server-holds-no-external-address.md)。

HTTPS 响应会带 HSTS，登录和退出 Cookie 使用 `Secure`；HTTP 本地开发入口不设置这些 HTTPS 专用标记。
两种部署都返回防嵌套、MIME 嗅探、引用来源与权限策略头。全局 CSP 限制 `frame-ancestors`、`base-uri` 和
`object-src`；Playground 的沙箱预览保留自己的脚本与网络策略，避免继承全局脚本限制而失效。

### 原生 HTTPS

在 `server.yaml` 配置证书链与私钥，并使用本机具体 IP：

```yaml
listen:
  console: [192.0.2.10:8800] # 替换为本机实际地址
tls_cert_file: /etc/gpuworks/fullchain.pem
tls_key_file: /etc/gpuworks/privkey.pem
```

重启后，通过证书覆盖的域名访问 `https://域名:8800`，agent 使用对应 `https://` / `wss://` 地址。
证书或私钥读取失败、缺一项、或 TLS 使用通配监听时会拒绝启动。
原生 TLS 必须使用 `listen.console`；不要用 `--addr` 覆盖，它不会创建服务自调用所需的本机维护监听，启动时会被拒绝。
证书更新后重启生效。所有非维护监听（包括节点和推理入口）使用 TLS，公网明文请求不能登录；
`127.0.0.1` / `::1` 的本机维护入口仍使用 HTTP，供升级、安装和服务自调用，不能映射给外部用户。
原生 TLS 不会替你申请证书，也不会自动修改现有 agent 已保存的 server 地址。

### 反代配置要点（控制台）

浏览器的控制台写请求（登录、退出、各类修改）带 `Origin` 头，server 拿它和自己看到的
「协议 + 主机 + 端口」比对，不一致就拒绝，返回 403 `MANAGEMENT_ORIGIN_REJECTED`。反代必须把浏览器
访问用的主机和端口原样转给 server：

```nginx
server {
    listen 443 ssl;
    server_name gpuworks.example.com;             # 替换为实际域名
    ssl_certificate /etc/gpuworks/fullchain.pem;
    ssl_certificate_key /etc/gpuworks/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    location / {
        proxy_pass http://<server 地址>:8800;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;              # 带端口；$host 不带端口
        proxy_set_header X-Forwarded-Host $http_host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Port $server_port;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header Upgrade $http_upgrade;        # agent 接入、Web 终端走 WebSocket
        proxy_set_header Connection "upgrade";
        proxy_buffering off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

将明文 80 入口重定向到 HTTPS，并用防火墙或本机监听限制外部直接访问反代后的 HTTP 端口。
Nginx 的 `$host` 不含端口，反代监听非默认 HTTPS 端口（例如 8808）时，浏览器 Origin 是
`https://<地址>:8808`，转发的 Host 却只有 `<地址>`。反代地址在 `trusted_proxies` 里时，server 会用
`X-Forwarded-Port` 补回缺掉的端口，所以写 `$host` 也能用；反代不在 `trusted_proxies` 里时，server
只看 `Host`，必须写 `$http_host`。`trusted_proxies` 只写实际反代的地址，不要写 `0.0.0.0/0`：
那等于让任何来源都能伪造这些转发头。

### 反代配置要点（/v1 网关）

推理请求经过反代时，默认配置会破坏流式输出或截断长请求。以 Nginx 为例，`/v1/` 至少需要：

```nginx
location /v1/ {
    proxy_pass http://gpuworks;          # upstream 指向 server
    proxy_http_version 1.1;
    proxy_set_header Connection "";      # 与 upstream 复用长连接
    proxy_buffering off;                 # 否则 SSE 被攒批，TTFT 变成整段响应时间
    proxy_request_buffering off;         # 大请求体直接转发，不在 Nginx 再落一次盘
    client_max_body_size 256m;           # 与 --proxy-max-body-bytes 对齐（Nginx 默认 1m）
    proxy_read_timeout 3600s;            # 两次读之间的间隔；非流式长生成要等到整段完成
    proxy_send_timeout 3600s;
}
```

server 自身对 HTTP 连接不设读写超时（agent WebSocket 与 SSE 都是长连接），长流只会被中间的
反代、负载均衡或客户端 SDK 的超时切断。流式请求每个 token 都会重置 `proxy_read_timeout`；
**非流式**请求在生成结束前没有任何字节，数万 token 的生成需要把超时设到大于最长生成时间，
更推荐长生成一律用 `stream: true`。

## 网关容量与主机调优

`/v1` 数据面全部经过 server 进程：鉴权、选副本、经 agent 隧道转发请求体与流式响应。
容量按下面三项估算（数据来自 [400 实例压测](../../artifacts/archive/2026-09-24-gateway-load-acceptance/gateway-load-gpu2-2026-09-24/README.md)，
fake 引擎、同机回环，真实网络会更高）：

| 负载特征 | server CPU | 参考点 |
|---|---|---|
| 流式输出 | 约 35 µs / token（每个 SSE delta） | 4000 路并发、17 万 token/s 时 5.9 核 |
| 请求体上传 | 约 24 核 / (GB/s) | 2000 路并发、每请求 2.4 MB、约 380 MB/s 时 9.3 核 |
| 在途连接 | 约 0.2 MB 内存 / 流 | 4000 路长流时 RSS 约 850 MB |

例：400 个实例、每实例 16 路并发、每路 50 token/s，约 32 万 token/s，需要约 11 个核；
在此之上再按请求体带宽加核。网卡需要承载请求体的**两倍**流量（客户端进、转发到节点出）。

主机参数（server 所在机器）：

- `net.core.somaxconn` 调到 4096 或以上，改后重启 server（监听队列长度在启动时读取）。默认
  128 时，数千客户端同时建连会溢出监听队列，被丢的 SYN 要 1 s 后重传，表现为一批请求整整
  慢 1 s（`nstat TcpExtListenOverflows` 可见）。
- 文件描述符：server 启动时会把软限制提到硬限制（Go 运行时行为），所以要保证**硬限制**至少为
  「并发客户端数 × 2 + 节点数 × 4」再留余量。`gpuworks install` 生成的 unit 没有设置
  `LimitNOFILE`，沿用 systemd 默认值；手工部署或默认值偏小时在 unit 里显式设置。每个 `/v1`
  请求占用一个客户端连接，大请求体再占一个临时文件。
- 请求体临时文件：超过 1 MiB 或超出进程内 256 MiB 预算的请求体写入系统临时目录（Go 的
  `os.TempDir()`，即标准的 `TMPDIR` 环境变量，默认 `/tmp`），请求结束后删除。同时在途的大请求
  会在这里占用「并发数 × 请求体大小」的空间，XFS 对打开中的文件还有预分配，实测约放大到
  2.5 倍。长上下文业务应在 service 环境里把 `TMPDIR` 指向空间充足的数据盘，不要落在系统盘。

API key：网关对校验通过的 key 在内存缓存 5 分钟。通过管理 API 吊销 key 立即失效；只有绕过
server 直接改数据库时才要等缓存过期。每个调用方使用独立的 key，便于吊销和按 `last_used`
（分钟粒度）排查。

长请求与升级：server 重启或升级会断开全部在途的流，已经开始的响应不会重试；节点 agent
断线会断开该节点上的全部流。长生成业务的升级应安排在低峰，或先在调用方做好断流重试。

网关指标（在监控中心与 `/api/v1/metrics/query` 可查）：`gpuworks_proxy_requests_total`、
`gpuworks_proxy_request_duration_ms_*`、`gpuworks_proxy_requests_inflight`，以及副本分布
`gpuworks_proxy_routed_total{deployment,instance}` 与重试 `gpuworks_proxy_retries_total`。

## 凭证与备份

保护 data 目录中的 `master.key`（首次启动生成，权限 0664，与安装树共享给服务账户和操作员）；备份数据库和该 key，二者缺一
不可。首次输出的 admin 密码/API key 立即轮换，定期使用节点/网关
页面和 `reset-password` 更换凭证。`gpuworks install` 确定的初始密码记录在
`<data>/initial-credentials`（0664），改密后删除该文件。

## 运行与升级

使用独立 service 用户和持久化 data、模型盘；限制管理 API 与 `/dl/` 的网络访问。默认的
`gpuworks install` 已经按这个约定落地：专用系统用户 `gpuworks`、`<dir>/data` 持久化数据、
日志与备份目录齐备。升级走 `gpuworks upgrade`（停服 → 备份数据库/master.key/旧二进制 → 原子
替换 → 探活，失败自动回滚到快照）；手工部署或需要自定义产物时，按[发布指南](../dev/release.md)
原子替换并保留回滚二进制。数据库迁移只有前滚、没有 down 迁移，所以升级前一定要有备份：`gpuworks upgrade` 会自动备份，手工替换要自己备份
`gpuworks.db` 与 `master.key`；手工路径也没有自动探活与回滚。步骤和回滚方式见[升级与版本兼容](upgrade-and-compatibility.md)。

完整的安装、凭证、目录布局和回滚语义见[安装和升级控制面](install-server.md)。

