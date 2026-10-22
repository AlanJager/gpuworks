# 安全默认值清单

- 管理 API 使用持久化会话：浏览器保存 HttpOnly 不透明 Cookie，Bearer JWT 绑定同一会话；每次鉴权
  查询会话和当前用户权限，数据库只保存随机凭证的 SHA256。改密与撤销全部管理会话在同一事务中，
  退出撤销当前所提交凭证的会话。升级前未绑定会话的凭证需要重新登录。SSE 在转发消息前复查会话，
  SSE 和终端还通过撤销事件及每秒检查关闭失效连接；终端数据帧不逐帧查库，独立进程执行
  `reset-password` 后也会在下次检查时关闭连接。已完成鉴权并执行的请求不追溯回滚。
  /v1 使用独立 API key。per-node token 只用于该节点连接和 bootstrap，不在公共
  /install.sh 中注入。
- 管理 API 与 MCP 也接受平台 API 令牌（`gwp_`）：只存 SHA-256，按所属用户的当前角色执行再叠加范围（默认只读），
  可设有效期、可吊销；任何令牌都不能管理令牌。MCP 默认关闭，由管理员开启。
- AI/provider 凭证使用 server 的 AES-GCM secretbox 加密；master secret 只从 data 目录的
  `master.key`（首次启动生成）读取。
- install script 下载 agent 到临时文件后原子替换；systemd unit 和 state 目录按最小权限创建，
  identity.json 只在卸载时删除。agent 的启动配置（含节点 token）只记在 `<state>/gpuworks-agent.conf`，权限 0600，
  经 `gpuworks-agent configure` 从标准输入写入；systemd 单元、nohup 的 `agent-restart.sh`、launchd plist 里都只有
  `run --state <目录>`，token 不出现在 `ps`、`systemctl cat` 或 plist 里。卸载时该文件随 agent 一起删除。
  离线包 `/dl/gpuworks-agent.run` 是公开下载（与其他 `/dl/` 文件一样），不含 token，token 只在运行它的命令行里给出。
- root agent 以专用用户（`udocker`）运行 udocker/runveil 时，state 目录里既有该用户要执行的
  patched PRoot 与 runtime-mounts，也有 agent 私有的 identity 与日志。agent 每次使用 runtime 前
  只给 state 目录本身补"穿越"（x）权限，不给列目录或写入：目录的组没有任何权限（或已是专用用户
  主组）时改属专用用户主组并加 g+x（agent 自建的 0700 → 0710）；组已有自己的权限时不动组、只加
  o+x（0750 → 0751）。state 目录不属于 agent、可被组或他人写入时只报错、不修改；上层目录属于
  运维，只检查不修改：不可穿越时 udocker 初始化只记 warning（镜像管理与 runveil 排障命令照常可用，
  启动 workload 会失败），准备 runtime-mounts 时报错。私有文件保持自身的 0600。
- 控制面 `gpuworks install` 默认以专用系统用户 `gpuworks` 运行单元（`--service-user root` 才算
  显式退回）；首启密码只经 `<data>/initial-credentials`（0664）传递，单元文件是全局可读的，
  因此绝不写入密码。`gpuworks upgrade` 先校验发布包的 sha256 再原子替换，探活失败回滚到升级前
  快照；server 不存储也不推断自己的对外地址，节点安装命令里的地址由调用方（浏览器、API 调用方、
  `--server`）显式给出，见 [ADR 0025](../adr/0025-server-holds-no-external-address.md)。安装与升级都会写审计到
  `<data>/install-history.jsonl`（0600，仅操作员读写）。
- 只有显式配置的 IP/CIDR 才信任 X-Forwarded-For（用于记录 agent 接入连接的来源地址）；TLS 通常由受信任反代终止。
- server_dial 使用节点 token 派生的 HMAC challenge；临时 preflight/peer 端口使用一次性 token 与 TTL，
  完成后关闭。
- 服务网络转发（节点有服务网络时，agent 在服务 IP 的 `19443` 端口监听）：TLS 1.3，密钥由节点 token
  经 HMAC 派生，网关按公钥固定校验而不依赖 CA；每个连接必须先出示 `HMAC(节点 token, 实例 UUID)`，
  只能转到本节点正在运行的实例的回环端口。令牌不落库、不在界面显示；推理引擎端口仍只发布在
  `127.0.0.1`。重新生成节点 token 会同时轮换 TLS 密钥和全部实例令牌。
- 网关将 contract 外路径返回 404，睡眠/不可用实例返回 503；日志会脱敏外部命令和凭证。
- web terminal 受 kill switch、并发上限、空闲超时和审计策略约束。

安全模型中的取舍（反代 TLS 依赖、token 在目标进程可见性、信任边界）见安全模型说明。生产部署
还应备份数据库和 master key，并轮换 admin 密码/API key。

