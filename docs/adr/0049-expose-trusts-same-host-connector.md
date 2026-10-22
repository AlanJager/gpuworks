# ADR 0049: 公网入口启用期间，server 信任来自本机的转发头

日期：2026-10-17 · 状态：已接受（补充 [ADR 0045](0045-expose-connector-service.md)）

## 背景

`gpuworks expose` 的连接器（cloudflared）终止 HTTPS，以明文 HTTP 转发给 server，带上浏览器的 Host 和 `X-Forwarded-Proto: https`。server 只采信 `trusted_proxies` 中对端发来的转发头，否则把自己的 origin 算成 `http://<host>`。浏览器写请求的 Origin 是 `https://<host>`，两者对不上，登录直接返回 `MANAGEMENT_ORIGIN_REJECTED`。

`install` 默认写入的 `127.0.0.1/32, ::1/128` 只能覆盖一种情况：连接器指向 loopback，并且 server 是 install 装的。下面两种情况都会得到"页面打得开、登不进去"的入口（2026-10-17 在 gpu3:8816 上遇到）：

- 直接用 `gpuworks serve` 启动的 server，`trusted_proxies` 默认是空的；
- `expose up --origin` 指向本机的非 loopback 地址，这时连接器的对端是本机网卡地址，不在 loopback 网段里。

## 决策

1. **按"来自本机"判断，而不是按地址列表判断**：只要本数据目录的入口处于 `desired: enabled`，server 额外信任来自本机的连接。判定条件是对端是 loopback，或者对端地址等于这条连接到达的本地地址。连接器与 server 共用数据目录，所以一定在同一台机器上；无论 origin 写的是哪个本机地址，它都满足这个条件。外部主机伪造不了这样的源地址：内核会把源地址是本机地址的外来包当作 martian 丢掉，回包也到不了对方，TCP 握手无法完成。
2. **只在入口启用期间生效**：server 监视 `<data>/expose/config.json`，只有文件变化时才重新读取。`expose up` 之后立即开始信任，`expose down` / `rm` 之后立即停止，server 不用重启。没用 expose 的部署，行为与原来完全一样。
3. **只有一份信任判断**：控制台的 Origin 与 Secure Cookie 判断、agent hub 的 `X-Forwarded-For` 来源判断，共用同一个 `auth.ProxyTrust`。

## 取舍

- **入口启用期间，本机任何进程转发来的头都被采信**，例如 `ssh -L` 转发、本机的其他反代。这和 install 默认信任 loopback 是同一档。直连客户端本来就能自己写 Origin，伪造转发头既绕不过浏览器的同源保护，也拿不到更多权限，最多影响记录下来的来源 IP。
- **不改用 unix socket 专用通道**：named tunnel 的转发目标写在用户的 Cloudflare 路由里，我们改不了，只能按对端地址判断。
- **不把 loopback 改成 `serve` 的默认信任**：那会改变所有不用 expose 的部署的安全默认值，而且仍然覆盖不了 `--origin` 指向本机网卡地址的情况。
