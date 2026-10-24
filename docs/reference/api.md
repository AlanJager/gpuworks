# REST API 全量路由

管理面路由统一挂在 `/api/v1` 下，由 `internal/server/api.Router` 注册（chi router）；完整清单由
`internal/server/api/router_inventory_test.go` 的 `TestRouterInventoryMatchesCompleteContract` 对
实际路由树做逐条断言，新增/删除路由必须同步更新该测试。数据面 `/v1/*`（OpenAI 兼容网关，API
key 鉴权）与 `/api/v1/playground/v1/*`（Playground 内部代理）不在本文重复，见
[`openai-gateway.md`](openai-gateway.md)。

登录后的管理面响应中，`Content-Type: application/json` 的在请求带 `Accept-Encoding: gzip` 时以 gzip 压缩（千台节点、
三千个部署时节点与部署列表可达数 MB，压缩后约为十分之一）；事件流、日志、文件与镜像传输保持原样，WebSocket 升级不受影响。

## 鉴权模型

- 管理面用 JWT：登录后经 `Authorization: Bearer <token>` 或 `gpuworks_session` Cookie（`HttpOnly`，
  `SameSite=Lax`，24 小时过期）携带；`internal/server/api/auth.go` 的 `managementAuth` 中间件校验，
  缺失或无效返回 `401 AUTHENTICATION_REQUIRED`。
- 管理面也接受平台 API 令牌（`Authorization: Bearer gwp_…`，见下文「Platform Tokens」）：请求以令牌所属用户的当前角色执行，
  再叠加令牌范围。`read` 令牌只能 GET/HEAD 与 `POST /api/v1/mcp`，其余请求回 `403 PLATFORM_TOKEN_READ_ONLY`；
  任何令牌都不能访问 `/api/v1/platform-tokens`（`403 PLATFORM_TOKEN_NOT_ALLOWED`），令牌只能在登录会话里管理。
  令牌未知、已过期或所属用户已删除时回 `401 AUTHENTICATION_REQUIRED`。
- 管理员专属操作再叠加 `managementAdmin` 中间件，角色非 `admin` 返回 `403 ADMINISTRATOR_REQUIRED`。
  下表"鉴权"列的 `JWT` 表示登录用户即可，`JWT(admin)` 表示需要管理员角色，`无` 表示不校验管理 JWT。
- `GET /api/v1/agent/connect` 走 hub 的逐节点 token 握手（WS upgrade → yamux），不是管理 JWT。
- `GET /install.sh`、顶层 `GET /healthz`（区别于 `/api/v1/healthz`）不挂在 `/api/v1` 下，公开访问。
- `GET /dl/gpuworks-agent-<os>-<arch>` 是 agent 二进制下载；`GET /dl/gpuworks-agent.run` 是离线包（一个自解压 sh 脚本，含 linux-amd64、linux-arm64、darwin-arm64 的 agent 与 `install.sh`，不含 token），`GET /dl/gpuworks-agent.run.sha256` 是它的校验和（`sha256sum` 格式）。都不挂在 `/api/v1` 下，公开访问，由节点接入功能的监听地址提供；server 内嵌的 agent 一个都没有时离线包返回 404。
- `/v1/*`、`/v1/models`（`ProxyV1`/`ProxyModels`）与 `/api/v1/playground/v1/*`（`PlaygroundV1`）在
  `internal/server/app` 装配时始终注入非空 handler；`router_inventory_test.go` 用零值 `Deps{}`
  跑测试，因此这三个通配路由和 SPA fallback `/*` 不出现在其断言列表里，但生产环境始终挂载。

## 错误与响应约定

- 响应遵守 [API 规范](api-conventions.md)：字段名一律 snake_case；返回一组对象的接口一律返回
  `{"items": [...], "total": N}`（不分页时 `total` 等于 `items` 的长度），不返回裸数组；游标分页的接口
  返回 `{"items": [...]}`。`make check-api-conventions` 从类型上检查这两条。
- 错误响应固定形态：`{"error": "<message>"}`；命中 `messages.ProductError` 时额外带稳定
  `code`（如 `NODE_OFFLINE`、`MODEL_IN_USE`）与可选 `params`，供前端按 locale 翻译，定义见
  `internal/server/api/errors.go`（130+ 个稳定 code，不在此逐条罗列）。
- `GET /api/v1/events`、`GET /api/v1/instances/{uuid}/logs` 是 SSE（`Content-Type: text/event-stream`），
  不是普通 JSON 响应；events 只推送变更通知（不带负载），客户端据此重新拉取。
- 列表端点一般不分页，`items` 里是全部元素。`GET /api/v1/deployments` 与 `GET /api/v1/nodes` 例外：请求带
  `page`（从 1 起）或 `page_size`（1–200，默认 50）时返回一页（`GET /api/v1/models`、`GET /api/v1/gpus` 同样分页，参数见各自条目）
  `{"items": [...], "page", "page_size", "total"}`（`total` 为筛选后的命中数，最新优先），可加 `q`
  （名称子串，不区分大小写）与 `state`（逗号分隔，取值之间为“或”）筛选，`sort=<列>` 升序、`sort=-<列>` 降序（在筛选之后、分页之前对全部命中排序，相等的保持最新优先；部署可选 `name`、`model`、`state`、`replicas`、`created_at`，节点可选 `name`、`state`、`agent_version`、`gpu_count`、`cpu_cores`、`mem_total`、`created_at`）；不带这两个参数时返回全部命中的完整对象 `{"items": [...], "total"}`（筛选与排序参数同样生效，只是不分页），
  供脚本与程序化调用。`GET /api/v1/inference-templates` 总是分页（缺省第 1 页、每页 50），信封相同，另有自己的硬件与文本筛选。
  三者共用同一套参数校验：非法页码/页大小回 400 `LIST_PAGE_INVALID`（不再悄悄改成默认值），未知筛选值或排序列回 400 `LIST_FILTER_INVALID`。
  部署的一页只含列表列（`id`、`name`、`template_id`、`template_deviated`、`engine`、`image`、`model_id`、
  `model_name`、`served_name`、`node_uuid`、`node_name`、`node_names`、`desired_state`、`runtime_state`、
  `sleepable`、`replicas`、`operations`、`created_at`），启动配置、目录选择快照与实例只在
  `GET /api/v1/deployments/{id}` 返回；部署的 `q` 匹配名称、对外模型名与模型名，`state` 取界面显示的状态（`running`、`starting`、`pulling`、`pending`、
  `failed`、`stopped`、`deleting`，删除中优先于运行时状态）。节点的一页不含 `topology` 与 `path_usage`
  （见 `GET /api/v1/nodes/{uuid}`），另带 `all`（全部节点数）与 `labels`（全体节点的用户标签
  `[{"key", "values"}]`，不受筛选影响）；`q` 匹配名称、主机名与 uuid，`state` 取 `online`/`offline`/
  `connecting`（只说 agent 连没连上；加速卡的问题不改变它），`label=key=value` 可重复：同一个 key 的多个值之间为“或”，不同 key 之间为“且”。
  `label` 与部署的放置条件同一套匹配（`planning.MatchRequirements`）：`gpuworks.ai/` 开头的 key 匹配系统标签
  （如 `gpuworks.ai/vendor=nvidia`、`gpuworks.ai/gpu-model=A100`），其余匹配用户标签；不存在的系统 key 或非法取值回 400
  `LIST_FILTER_INVALID`。`GET /api/v1/nodes/facets` 接受同样的 `q`/`state`/`label`，返回命中节点的数量与它们携带的
  用户标签值、系统标签值（不含节点自身的 `gpuworks.ai/node`，带服务端显示文本）和卡型号（`vendor`/`name`/`cc`，
  `model` 即 `gpuworks.ai/gpu-model` 的取值，另带 `cards` 卡数与 `memory_bytes` 最大单卡显存），每项附带携带它的节点数；部署表单与放置条件编辑据此给出选项与计数，
  不再读取全部节点。
- 其余列表中，`GET /api/v1/logs/search` 用 `limit`（默认 200，上限
  5000）加 `cursor_ts`/`cursor_id` 游标翻页（`with_total=1` 时附总命中数），`GET /api/v1/metrics/query`
  用 `range`/`step`（Go duration 字符串，默认 6h/1m）加 `node`/`gpu`/`gpu_uuid`/`instance`/`benchmark`/`point` matcher。
- 两个事件时间线端点（`/api/v1/instances/{uuid}/events`、`/api/v1/deployments/{id}/events`）是
  **最新优先**的游标分页：`limit`（默认 100，上限 500）与 `before=<上一页最后一条的 id>`，
  响应为 `{"items": [...]}`。非法游标/页大小回 400（`EVENT_CURSOR_INVALID` / `EVENT_LIMIT_INVALID`）。
  响应**读取时折叠**：同一实例内 (kind, code, state, phase) 相同的连续行只返回其中最新的一条，
  因此旧库里成千上万行进度上报读出来仍然是可读的里程碑序列。带 `code` 的行不再重复下发
  `data` 原文（内容已在 `params` 里）；早于 code 机制的历史行仍然带 `data`。

## Auth

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| POST | `/api/v1/auth/login` | 用户名密码登录，创建持久化会话；返回绑定会话的 JWT 并写不透明 Cookie | 无 |
| POST | `/api/v1/auth/logout` | 撤销所提交凭证对应的会话并清除 Cookie；重复退出幂等 | 无 |
| GET | `/api/v1/auth/me` | 返回当前用户与实时权限；不延长空闲期限 | 管理会话或平台 API Token |
| POST | `/api/v1/auth/activity` | 报告真实操作并延长空闲期限；无请求体，成功返回 204 | 管理会话 |

管理会话接受 `gpuworks_session` HttpOnly Cookie，或登录响应中的 Bearer JWT。下表沿用 JWT 作为管理鉴权的简写；两者都必须关联仍有效的持久化会话。平台 API Token（`gwp_`）继续按范围和有效期独立鉴权；它不能调用 `auth/activity`，密码重置不撤销平台 API Token。Bearer 优先于 Cookie，无效 Bearer 不会降级使用 Cookie。密码重置和退出会使已签发的 JWT 同时失效；升级前未绑定会话的 JWT 和旧 Cookie 需要重新登录。

`auth/me` 响应仍为 `{"id":1,"username":"admin","role":"admin"}`；`auth/login` 仍为 `{"token":"<JWT>","user":{"id":1,"username":"admin","role":"admin"}}`。会话失效返回 401、稳定错误码 `AUTHENTICATION_REQUIRED`；存储不可用返回 503。登录、退出和使用登录会话的管理写请求拒绝跨来源请求，返回 403、`MANAGEMENT_ORIGIN_REJECTED`。

## Nodes（节点接入）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/agent/connect` | agent 长连接 WS upgrade 入口 | 节点 token（非管理 JWT） |
| GET | `/install.sh` | curl\|sh 安装脚本 | 无 |
| GET | `/api/v1/healthz` | 健康检查（管理面） | JWT |
| GET | `/healthz` | 健康检查（无需登录，供探活/负载均衡） | 无 |
| GET | `/api/v1/engines` | 引擎参数字典（前端渲染表单用） | JWT |
| GET | `/api/v1/nodes` | 节点列表；带 `page`/`page_size` 时分页并可按 `q`/`state`/`label` 筛选（见“错误与响应约定”） | JWT |
| GET | `/api/v1/nodes/facets` | 按 `q`/`state`/`label` 命中的节点数与其用户标签、系统标签、卡型号的分布（见“错误与响应约定”） | JWT |
| GET | `/api/v1/dashboard/summary` | 仪表盘汇总：节点按状态计数与硬件合计、部署按状态计数、按厂商与按标签（`label_key`，缺省或无节点带该 key 时取最常用的 key）分组的容量 | JWT |
| POST | `/api/v1/nodes` | 创建待接入节点，返回安装命令与 token；`connect_mode` 为 `agent_dial`（默认）时请求体必须带 `server_url`（agent 将拨号的地址，UI 用浏览器当前地址预填），缺失返回 400 `SERVER_URL_REQUIRED`，不是不含账号/查询/片段的绝对 http(s) 地址返回 400 `SERVER_URL_INVALID`；`server_dial` 时 `server_url` 可省略，传了也不保存（agent 从不连接 server），响应里的 `server_url` 为空，`install_command` 是[离线包](../how-to/connect-nodes.md#离线包)的命令（`sh gpuworks-agent.run --token '…' --connect-mode server_dial --listen '…'`）。`server_dial_listen` 缺省为 `0.0.0.0:18800`，不是有效的 `host:port` / `:port`（端口 1–65535）返回 400 `SERVER_DIAL_LISTEN_INVALID`（`address`） | JWT(admin) |
| GET | `/api/v1/agent/rejects` | 最近被拒绝的连接尝试 | JWT |
| GET | `/api/v1/agent/upgrade-errors` | 最近 agent 升级失败记录 | JWT |
| POST | `/api/v1/nodes/precheck` | SSH 纳管前置检查。除主机环境外还检查节点与 server 的双向连通：`Server reachability`（节点 → server）、`Server to agent`（server 拨 `host_address`，缺省为 SSH 主机；`host_address` 自带端口时用它，否则取 `server_dial_listen` 的端口，默认 `0.0.0.0:18800`；`server_dial` 时不通为错误而非警告）和结论行 `Connectivity`，见[节点与 server 网络不通](../how-to/connect-nodes.md#节点与-server-网络不通) | JWT(admin) |
| POST | `/api/v1/nodes/install` | 经 SSH 远程安装 agent（Linux 与 Mac 都是上传 agent 和 `install.sh` 后运行脚本）；`agent_dial` 同样要求 `server_url`，`server_dial` 不需要，校验规则与错误码同上，`server_dial_listen` 的校验同上。`install_dir` 省略时用 `/opt/gpuworks`，写不进时回退到 `/tmp/gpuworks`；填了的目录写不进时安装失败，不回退。`no_systemd: true` 让 Linux agent 以 nohup 运行而不注册 `gpuworks-agent.service`，不动主机上已有的服务与其他目录的 agent：此时不检查「主机已是节点」，只有安装目录里已有 agent 在运行时回 409 `NODE_INSTALL_EXISTING_AGENT_SAME_DIR`（`dir`），带 `replace_existing_agent` 重试即替换它；省略时默认用 systemd；macOS 忽略此项 | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}` | 节点详情（含 GPU 列表；管理员额外含 token） | JWT |
| GET | `/api/v1/nodes/{uuid}/fs` | 节点文件系统浏览（本地模型注册用） | JWT |
| POST | `/api/v1/nodes/{uuid}/doctor` | 触发节点自检 RPC | JWT |
| POST | `/api/v1/nodes/{uuid}/upgrade` | 触发 agent 远程升级 | JWT |
| PUT | `/api/v1/nodes/{uuid}/scheduling` | 停止调度、驱逐或恢复调度：`{"scheduling": "cordoned" \| "draining" \| "schedulable"}`，返回节点（节点对象带 `scheduling`）。`cordoned` 不再往这台节点启动新副本（候选池里单列为 `cordoned`，指定它的多机、PD rank 等待，原因 `node_cordoned`），已有副本照常服务；`draining` 还会把候选池放过来的副本逐个迁走：先在别的节点起好替代副本，替代副本运行后原副本才排空退役，每个部署一次一个；`schedulable` 恢复，等待中的副本立即重试。其它值 400 `FIELD_INVALID` | JWT |
| GET | `/api/v1/nodes/{uuid}/replicas` | 节点上仍在运行的实例与驱逐对它的处理：`{"scheduling", "items": [{"instance", "deployment_id", "deployment", "state", "kind"}]}`，`kind` 为 `pool`（迁到别的节点）、`pinned`（部署只钉在这台，等待）或 `named`（多机、PD 的 rank，只有强制驱逐才撤下）；已停止、已失败的实例不列 | JWT |
| POST | `/api/v1/nodes/{uuid}/evict` | 强制驱逐：不等替代副本，把节点上每个副本立即撤下（所在副本的 leader 进入排空，follower 随之），副本在能放的地方重建，不计入重试次数；返回 `{"evicted": [leader UUID…]}`（202）。节点仍可调度时 409 `NODE_SCHEDULABLE` | JWT |
| PUT | `/api/v1/nodes/{uuid}/labels` | 替换节点标签（key/value；空对象清空）。键名不得使用平台保留的 `gpuworks.ai/` 命名空间及其子域 | JWT |
| PATCH | `/api/v1/nodes/{uuid}/name` | 重命名节点 | JWT |
| PATCH | `/api/v1/nodes/{uuid}/accelerator` | 一机多厂商时选择节点管理哪家的卡：`{"accelerator": "auto" \| "nvidia" \| "ascend" \| "hygon" \| "ppu"}`，返回节点（带 `accelerator_preference`；`accelerator_vendors` 是 agent 上报的主机上每个厂商的 `{vendor, cards, model}`）。agent 接受后原地重启改管那家的卡，选择才写入。非法值 400 `NODE_ACCELERATOR_INVALID`，主机上没有该厂商的卡 400 `NODE_ACCELERATOR_NOT_ON_HOST`，节点上有未停止的实例 409 `NODE_ACCELERATOR_HAS_INSTANCES`（`params.count`），agent 拒绝或离线 409；节点不存在 404 | JWT |
| GET | `/api/v1/nodes/{uuid}/pull-proxy` | 节点使用的代理：`proxy`（首个选中它的代理条目，形同 `/pull/proxies` 的一项；没有条目选中时为 `null`，节点 agent 用自身环境）与 `runtime`：运行时类型，docker 节点另有 dockerd 实际使用的代理、与该条目是否一致（`applied`；节点没有代理时比较 systemd 当前加载的配置 `configured_*` 与实际值）、live-restore、`daemon.json` 是否也配了代理，以及 agent 的权限（`root` / `sudo` / `none`） | JWT |
| GET | `/api/v1/nodes/{uuid}/network` | 已删除：410 `SETTING_REMOVED`（`route` 指向 `/api/v1/pull/proxies`）。节点的代理与站点地址都在拉取设置里按节点标签配置（`/nodes/{uuid}/pull-proxy` 给出节点使用哪一条） | JWT |
| PUT | `/api/v1/nodes/{uuid}/network` | 已删除：410 `SETTING_REMOVED`，同上 | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/runtime/restart` | 重启节点的 dockerd 使代理生效，由 agent 执行（需 root 或免密 sudo）；失败 `RUNTIME_RESTART_FAILED`。dockerd 未开 live-restore 时节点上的容器会随之停止 | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}/networks` | 节点每个角色解析到的网络（网卡/IP/HCA/提示）、管理网（地址、NAT、关闭的角色）与上报的网卡、RDMA 端口清单（带最近两分钟的收发速率）和加速卡网口清单（`accelerator_ports`） | JWT |
| GET | `/api/v1/nodes/{uuid}/ssh` | 查看节点 SSH 凭据配置（不含明文密钥） | JWT |
| PUT | `/api/v1/nodes/{uuid}/ssh` | 写入节点 SSH 凭据（离线恢复用） | JWT(admin) |
| DELETE | `/api/v1/nodes/{uuid}/ssh` | 删除节点 SSH 凭据 | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/recovery/probe` | 手动触发一次离线节点恢复探测 | JWT(admin) |
| POST | `/api/v1/nodes/{uuid}/token/regenerate` | 重新生成节点 token（会踢掉当前连接） | JWT(admin) |
| DELETE | `/api/v1/nodes/{uuid}` | 默认通过保存的 SSH 凭据卸载 agent，确认完成后删除记录，返回 `{"status":"deleted","agent_uninstalled":true}`。卸载未确认回 409 `NODE_UNINSTALL_UNCONFIRMED`，保留节点及 SSH 凭据。显式 `?force=true` 跳过卸载，仅删除记录，返回 `agent_uninstalled:false`；两种方式均拒绝有实例的节点（409）。force 非布尔值或重复参数回 400；节点不存在回 404 | JWT |
| GET | `/api/v1/events` | SSE 变更事件流 | JWT |

### 节点标签：写入的与生成的

节点有两种标签，在 DTO 里是两个字段：

- `labels`：运维写的（`PUT /api/v1/nodes/{uuid}/labels`），键值自定。
- `system_labels`：平台从节点自身事实算出来的，只读。不落库，每次读取现算，所以换卡、改网络、
  重命名网络之后立刻反映出来。

生成的键都在保留命名空间 `gpuworks.ai/` 下，写入的标签不得使用该命名空间及其子域
（`rack.gpuworks.ai/...`），这样看键名就知道是平台的事实还是人写的。网络加载用的标签同此规则。
升级到引入这条规则的版本时，迁移 119 删除节点上已有的这类键（不分大小写），按这类键加载的网络
一并删除；部署里引用这类键的标签条件不改，因为删掉条件等于放宽约束——这样的部署在有人修改它之前
不会被调度到任何节点。

| 键 | 值 | 说明 |
|---|---|---|
| `gpuworks.ai/node` | 节点 UUID | `display` 是节点名 |
| `gpuworks.ai/vendor` | 规范厂商 id | 整机一个 |
| `gpuworks.ai/cpu-arch` | `amd64` / `arm64` | |
| `gpuworks.ai/gpu-model` | 规范化后的型号 | 每种型号一条，混卡节点有多条 |
| `gpuworks.ai/gpu-arch` | 厂商内的架构代际（`turing`、`910b`…） | 认不出的卡不出这条 |
| `gpuworks.ai/compute-network` | 网络 ID；管理网承担时为 `management` | 该角色无网络时不出这条 |
| `gpuworks.ai/storage-sync-network` | 同上 | |
| `gpuworks.ai/serving-network` | 同上 | |

同一个键可以有多条（值是集合）。每条是 `{key, value, display, display_code}`：`value` 是用于匹配的
字面值，`display` 是展示文本，`display_code` 只在值是平台词汇（厂商、管理网）时给出，指向客户端
已有的翻译；客户端优先用 `display_code`，取不到时用 `display`。网络和节点的值是 ID 而不是名字，
所以改名只改 `display`，不影响按标签做的匹配。

## Models（模型）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/models` | 模型列表。不带分页参数时返回全部模型（含逐节点/存储的就位明细）；带 `page`/`page_size` 时返回一页 `{items, page, page_size, total, summary}`：行不含 `readme`、`nodes`、`storages`，改带 `presence{ready_nodes, cached_nodes, ready_storages, in_flight, in_flight_percent, failed}` 汇总，`summary{registered, ready_files, sources, weight_gib}` 统计全部模型、不受筛选影响；可按 `q`（名称/来源 ID/修订子串）、`source`（`huggingface`/`modelscope`/`url`/`local`，逗号分隔）、`operation`（能力类型，逗号分隔，有其一即命中）、`source_id`（来源 ID 整体匹配、不区分大小写，逗号分隔；部署页按模板权重源查已注册模型用）筛选，`sort` 可选 `name`、`source`、`source_id`、`published_at`（源站创建时间，缺失时取最近更新）、`created_at`、`weight`、`presence`（就位节点数）；逐位置明细见 `GET /api/v1/models/{id}` | JWT |
| POST | `/api/v1/models` | 注册公共仓库模型（HF/ModelScope） | JWT |
| POST | `/api/v1/models/preview-url` | 预览 HTTP 模型目录（`url`）；必须为带主机名的 http/https URL，不允许内嵌凭据。非法地址返回 400 `MODEL_SOURCE_URL_INVALID`，合法但不可读取的目录返回 502 `HTTP_DIRECTORY_UNREACHABLE` | JWT |
| POST | `/api/v1/models/register-url` | 注册 HTTP 模型目录（`source_url`，可选 `source_id`、`name`）；地址校验与 preview-url 相同，注册成功返回 201 | JWT |
| POST | `/api/v1/models/local/preview` | 预览节点本地目录是否可注册为模型 | JWT |
| POST | `/api/v1/models/local` | 注册节点本地模型 | JWT |
| GET | `/api/v1/models/{id}/logo` | 模型 logo 图片 | JWT |
| GET | `/api/v1/models/{id}` | 模型详情 | JWT |
| DELETE | `/api/v1/models/{id}` | 删除模型 | JWT(admin) |
| POST | `/api/v1/models/{id}/metadata/refresh` | 重新拉取模型元数据（config/README/logo） | JWT |
| POST | `/api/v1/models/{id}/download` | 发起公共仓库下载：`storage_id` 选存储（省略为默认存储），本地存储再给 `node_uuid` 或 `node_uuids`，共享存储由一台写入节点下载一次 | JWT |
| POST | `/api/v1/models/{id}/copy` | 在两个位置之间复制模型文件：`source`/`target` 各为 `{storage_id, node_uuid?}`；同一节点能读源写目标时本机复制，否则节点间直传；源与目标为同一位置返回 400，共享存储按存储 ID 判定（忽略节点）；离线或不可写等状态冲突仍返回 409 | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/cache/retry` | 重试失败的 NFS Cache 复制（准备视图 `preparation.retryable` 为真时提供）；请求 `{storage_id}`，controller 先容量检查与旧 worker 退出确认，再写新复制意图并下发，接受返回 202；非失败任务、节点未连接、容量不足、旧任务未确认退出或并发变化返回 409 `MODEL_CACHE_RETRY_BLOCKED` | JWT |
| DELETE | `/api/v1/models/{id}/storages/{storage_id}` | 从共享存储删除模型文件（及 NFS 加缓存的各节点缓存）；别的 gpuworks 在用时 409 `STORAGE_SHARED_BY_OTHER_CONTROL_PLANES`，`?force=1` 强制 | JWT(admin) |
| POST | `/api/v1/models/{id}/register-path` | 将节点已有路径登记为该模型的本地文件 | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync` | 从显式选定的源节点做节点间直传同步（`source_node`、可选 `rate_limit_kib_s`；不带源节点时沿用失败记录的源节点重试） | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync/probe` | 测试源节点到目标节点的直连握手，不传字节 | JWT |
| POST | `/api/v1/models/{id}/nodes/{uuid}/sync/cancel` | 取消节点间同步 | JWT |
| DELETE | `/api/v1/models/{id}/nodes/{uuid}` | 驱逐节点上的模型文件（本地存储或节点缓存，`?storage_id=` 指定存储）（拒绝有活跃 deployment 占用的节点）；该路径上进行中的下载先被取消并等其结束，返回后可立即重新下载 | JWT(admin) |

### 登记节点已有路径

`POST /api/v1/models/{id}/register-path` 请求为
`{"node_uuid":"…","path":"/data/existing-model","storage_id":1}`。
`path` 是节点已有目录的宿主机路径，调用方应提供绝对路径；允许位于所选本地存储根目录之外。
`storage_id` 选择记录归属，不是路径范围限制；省略时优先采用该模型在节点上已有副本的存储，否则取默认本地存储。
该操作只登记已有文件，不复制或搬迁目录。共享存储的库导入使用存储扫描/导入接口。

成功返回 `200 {"status":"registered"}`。节点检查目录可读、有非空文件且没有未完成下载；
不要求固定权重文件名或 `config.json`，保留自定义引擎布局。`registered` 表示调用方选定的路径已登记，
不证明其中是正确的模型或引擎可以加载。不能用“路径在存储范围外”判断请求无效。

空 `path` 返回 400；目标模型不存在返回 404；缺失、空目录或含 `.part`/`.part.state` 的目录由节点拒绝，
接口返回 409，已有登记不被替换。真实节点 RPC 不可用等错误也可能返回 409，调用方应结合错误内容处理。

## Images / Storages（镜像目录与存储策略）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/images` | 镜像系列列表 | JWT |
| GET | `/api/v1/images/versions` | 镜像版本行列表；每行带 `present_nodes`/`total_nodes` 与 `reference_count`，不带逐节点行（节点行见系列详情） | JWT |
| GET | `/api/v1/images/engines` | 镜像表单可选的引擎字典 `[{"name","label"}]`，按展示顺序：全部部署引擎加目录专用的 `vllm-omni` | JWT |
| GET | `/api/v1/images/community/repos` | 社区镜像仓库卡片（内置六类与 `image_community_catalog` 设置合并后的结果，不含 tag） | JWT |
| GET | `/api/v1/images/community/browse` | `?repo=` 列出该仓库上游的全部 tag（任意仓库均可，不做 semver 过滤；内存缓存 1 小时）；仓库名为空或格式错误返回 400，上游报告仓库不存在返回 404 | JWT |
| PATCH | `/api/v1/images/versions/{id}` | 修改镜像版本行：只接受 `enabled` 与 `registry_sources`（系统版本也可加获取地址）；其他键回 400 `IMAGE_VERSION_PATCH_INVALID`，镜像目录不承载启动配置（ADR 0031） | JWT(admin) |
| GET | `/api/v1/images/{id}` | 镜像系列详情；各版本的节点行带 `node_name`、`node_state`、`image_digest_id`、`updated_at`，节点间同步来的行另带 `source_node_name` | JWT |
| PATCH | `/api/v1/images/{id}` | 修改镜像系列：`enabled`；社区/自定义系列另可改 `common_args`/`common_env`（系统系列 400 `OFFICIAL_IMAGE_SERIES_MANAGED`） | JWT(admin) |
| DELETE | `/api/v1/images/{id}` | 删除社区/自定义镜像版本；系统镜像 400 `OFFICIAL_IMAGE_DELETE_FORBIDDEN`，仍被部署、未结束的实例或模型引用时 409 `IMAGE_IN_USE`（与系统镜像删除/弃用同一引用口径） | JWT(admin) |
| DELETE | `/api/v1/image-series/{id}` | 删除社区/自定义镜像系列；任一版本仍被引用时 409 `IMAGE_IN_USE` | JWT(admin) |
| POST | `/api/v1/images/import` | 两种请求体：带 `schema_version` 的系统目录 manifest（schema 2/3，其他值 400），或社区/自定义添加用的 `{"series":[...]}`（不能写 `source=official`） | JWT(admin) |
| POST | `/api/v1/images/sync` | 「检查更新」：从发布通道取系统镜像目录并导入；请求体忽略。发布源都不可用 502 `UPDATE_SOURCES_UNAVAILABLE`，索引可用但没有源提供与索引一致的目录文件 502 `UPDATE_DOWNLOAD_FAILED`，渠道未发布 404 `UPDATE_NOT_PUBLISHED` | JWT(admin) |
| POST | `/api/v1/images/preview-archive-url` | 下载前解析 `docker save` 归档 URL：`{"url"}` → `url`、`size_bytes`、`repo_tags`、`previewable` | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync` | 触发节点镜像获取：空请求体按[拉取设置](../how-to/configure-pull-settings.md)拉取（与部署启动同一份候选，不能指定仓库）；`source: url` / `url-relay` 从 HTTP 归档导入；或从 `source_node` 节点间直连同步（可选 `rate_limit_kib_s`，不受同步组限制）。`source` 取 `""`、`default`、`url`、`url-relay` 以外的值回 400 `IMAGE_SOURCE_REMOVED`；来源规则让该节点无处可拉时回 409 `IMAGE_SOURCE_FORBIDDEN`；`"force":true` 时节点上已 `present` 也重新拉取 | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/probe` | 测试 `source_node` 到目标节点的直连握手，不传字节 | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/sync/retry` | 重试节点镜像获取；节点间同步沿用记录的源节点与上次限速 | JWT |
| POST | `/api/v1/images/{id}/nodes/{uuid}/cancel` | 取消该节点上进行中的镜像传输 | JWT |
| DELETE | `/api/v1/images/{id}/nodes/{uuid}` | 从节点删除该镜像 | JWT(admin) |
| POST | `/api/v1/images/{id}/nodes/{uuid}/inspect` | 读取节点上镜像的 OCI 配置（`entrypoint`/`cmd`/`env`/`volumes`）与镜像 id；配置只在响应里返回，不写回目录 | JWT(admin) |
| GET | `/api/v1/storages` | 存储列表（类型、路径或导出、节点汇总与问题节点、模型数、在用部署） | JWT |
| GET | `/api/v1/storages/{id}` | 单个存储；`?all_nodes=1` 返回每个节点的状态 | JWT |
| POST | `/api/v1/storages` | 添加存储；NFS 类先在一台节点上试挂，写不进去的保存为停用（只读） | admin |
| PATCH | `/api/v1/storages/{id}` | 修改名称、启用、默认、节点范围、路径、挂载选项、缓存目录与上限、`nfs`↔`nfs_cache` 类型；可带读取时的 `expected_updated_at` 防止覆盖并发修改，版本变化返回409 `STORAGE_CONFIG_CHANGED` | admin |
| DELETE | `/api/v1/storages/{id}` | 移除存储对象（不删文件）；有部署绑定时 409 `STORAGE_IN_USE` | admin |
| POST | `/api/v1/storages/probe` | 在一台节点上试挂一个存储定义，不留下挂载 | admin |
| GET | `/api/v1/storages/{id}/control-planes` | 共享存储上还有哪些 gpuworks 在用（心跳） | JWT |
| PATCH | `/api/v1/storages/{id}/node-overrides` | 原子修改固定 `node_uuids` 的缓存例外；`cache_path`/`cache_max_bytes` 缺省保持、null继承，限额0表示不限；新例外不扩大节点范围 | admin |
| POST | `/api/v1/storages/{id}/cache/reconcile` | `{node_uuids,mode:"trim"或"clear"}` 提交后台缓存整理，202返回accepted数量；逐节点结果见存储详情cache状态 | admin |
| GET | `/api/v1/storages/{id}/model-files` | 存储里的模型文件：共享存储每个模型一行，本地存储与 `nfs_cache` 的节点缓存每台节点一行（含最近使用时间与绑定的部署） | JWT |
| POST | `/api/v1/storages/{id}/scan` | 扫描共享存储里的模型目录，分页（`cursor`） | admin |
| POST | `/api/v1/storages/{id}/import` | 导入扫描到的目录，只写数据库 | admin |
| DELETE | `/api/v1/storages/{id}/cache` | 淘汰 `nfs_cache` 存储所有可淘汰的节点缓存，`?node_uuid=` 限一台 | admin |
| GET | `/api/v1/storages/{id}/files` | 浏览存储里的文件（[ADR 0053](../adr/0053-storage-files.md)）：`?path=`（相对存储根，空为根）`&cursor=&limit=`（每页至多 500，按名字字节序，`next_cursor` 为空即最后一页）；返回 `{path, node_uuid, writable, items[{name, path, dir, symlink, size, mtime, writable}], next_cursor}`。顶层 `writable` 表示当前目录能否新建子项，条目 `writable` 表示能否改名/删除；符号链接只列出、不跟随。共享存储可省略 `node_uuid`（自动选在线、ready、agent 支持且数据通道已连接的节点，优先可写节点），本地存储必填，否则 400 `STORAGE_FILE_NODE_REQUIRED`；`nfs_cache` 浏览的是 NFS 本身，不是节点缓存 | admin |
| GET | `/api/v1/storages/{id}/files/download` | 下载文件或目录：`?node_uuid=&path=&format=zip\|tar.gz`。文件原样返回（`Content-Length`、`application/octet-stream`）；目录边打包边传（默认 zip，`application/zip` 或 `application/gzip`，无 `Content-Length`），符号链接跳过并在包内 `SKIPPED_SYMLINKS.txt` 列出；`Content-Disposition: attachment` 带文件名（目录加 `.zip`/`.tar.gz`）。开始传字节之前的错误返回 JSON，之后出错直接断开连接。存储停用时仍可下载 | admin |
| PUT | `/api/v1/storages/{id}/files/upload` | 单请求流式上传：`?node_uuid=&path=&extract=tar\|tar.gz\|zip&overwrite=true\|false`，请求体为文件字节（`application/octet-stream`）。`path` 不解压时是目标文件，解压时是解压到的目标目录。server 不落盘、不缓存，`Content-Length` 作为预期大小（缺省为未知）；agent 先写入 `.gpuworks/incoming/` 暂存，完成（含解压）后原子改名到目标再回结果，返回 `{path, bytes, files}`。所有不需要 agent 的检查都在读请求体之前完成 | admin |
| POST | `/api/v1/storages/{id}/files/mkdir` | `{node_uuid, path}` 新建目录（含缺失的上级），已存在的目录不报错；返回 `{path}` | admin |
| POST | `/api/v1/storages/{id}/files/rename` | `{node_uuid, from, to}` 改名或移动，两端都须在可写区域、目标不得存在；返回 `{path}` | admin |
| DELETE | `/api/v1/storages/{id}/files` | `?node_uuid=&path=` 删除文件或目录：先移进存储的回收目录，后台删除字节；204 | admin |
| GET | `/api/v1/storages/settings` | 控制面显示名与缓存盘保留空间 | JWT |
| PATCH | `/api/v1/storages/settings` | 修改上述设置 | admin |
| GET | `/api/v1/storages/usage` | 各节点模型盘用量统计 | JWT |

存储返回 `updated_at` 作为编辑版本；target的 `override_only:true` 表示纯节点配置例外，不参与范围选择，省略或false保留历史显式选择语义。NFS缓存节点状态可带 `cache{used_bytes,reclaimable_bytes,protected_bytes,limit_bytes,state,reason?,message?}`；state为 `pending/running/failed/offline/blocked/satisfied/unavailable`。cache缺省表示统计未知，客户端不能当成0。目录未验证时旧磁盘容量不会作为新目录容量返回。

存储文件的写操作（上传、新建目录、改名、删除）只在 `files/` 与 `.gpuworks/apps/` 之下进行，区域目录本身不可删改，否则 403 `STORAGE_FILE_REGION_READONLY`。错误码：
路径非法（含 `..`、反斜杠、解析后穿出存储根）400 `STORAGE_FILE_PATH_INVALID`；不存在 404 `STORAGE_FILE_NOT_FOUND`；目标已存在 409 `STORAGE_FILE_EXISTS`；
节点上的 agent 不支持 409 `STORAGE_FILE_AGENT_UNSUPPORTED`（`params.node`）；解压失败 422 `STORAGE_FILE_EXTRACT_FAILED`；
上传/下载时节点数据通道未连接 503 `STORAGE_FILE_DATA_LANE_UNAVAILABLE`（可重试）；其他节点侧失败 502 `STORAGE_FILE_OPERATION_FAILED`；
另沿用 `STORAGE_DISABLED`（409，写操作）、`STORAGE_NOT_WRITABLE`、`NODE_OFFLINE`、`STORAGE_UNAVAILABLE_ON_NODE`（均 409）。

系统目录导入（`POST /images/sync` 与带 `schema_version` 的 `POST /images/import`）成功时返回
`{"catalog_version","created","updated","deleted","deprecated","skipped":[],"series":[...]}`：计数按版本行，
`series` 是导入后的系统系列；manifest 里没有的系统版本无引用即删除、有引用则弃用。`POST /images/sync` 从发布通道取目录：所有发布源都不可达 502
`UPDATE_SOURCES_UNAVAILABLE`、索引可用但没有源提供与索引一致的目录文件 502 `UPDATE_DOWNLOAD_FAILED`、通道没有发布镜像目录 404 `UPDATE_NOT_PUBLISHED`（库都不动），内容非法 400 `IMAGE_MANIFEST_INVALID`，与社区/自定义行同名或同 ref
冲突 400 `IMAGE_IMPORT_CONFLICT`（整次不写入）。社区/自定义表单返回受影响的系列 `{"items": [...], "total"}`。版本行带 `rolling_tag`、
`deprecated`、`deprecated_since` 与 `reference_count`（引用该 ref 的部署、未结束实例与模型数）。manifest 格式、
导入规则与滚动 tag 见[镜像目录与推理镜像家族](image-families.md)。

## Inference Templates（推理目录/模板）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/inference-templates` | 推理模板列表 | JWT |
| GET | `/api/v1/inference-templates/index` | 模板索引（轻量列表，用于选择器） | JWT |
| GET | `/api/v1/inference-templates/organizations` | 模板涉及的组织/发布方列表 | JWT |
| GET | `/api/v1/inference-templates/chips` | 模板声明支持的加速卡列表 | JWT |
| GET | `/api/v1/inference-templates/my-chips` | 当前集群实际探测到的加速卡列表 | JWT |
| GET | `/api/v1/inference-templates/summary` | 模板统计摘要 | JWT |
| GET | `/api/v1/inference-templates/export/{digest}` | 按内容摘要导出可执行模板包 | JWT(admin) |
| GET | `/api/v1/inference-templates/model/*` | 按模型路径获取模板详情（通配路径） | JWT |
| GET | `/api/v1/inference-templates/yaml?model_id=…` | 模板编辑器里的平铺 YAML：server 用导入同一套目录类型写出，可加 `spec_source=user`（或 `upstream`）只留该来源的 spec，`readme_*` 字段只在 spec 全部列出或模板本身归用户时写出；原样保存（`import?overwrite=true`）不丢字段 | JWT |
| GET | `/api/v1/inference-templates/schema` | 平铺 YAML 编辑器的字典：字段的补全、悬停说明与客户端首轮校验所需的约束（权威校验仍在导入时） | JWT |
| POST | `/api/v1/inference-templates/clone` | 克隆模板为可编辑的用户 spec | JWT |
| POST | `/api/v1/inference-templates/evaluate` | 只检查、不写入：请求体与 `import` 相同（平铺 YAML，或 `.gpuworks-catalog.tar.gz` 目录包；同样认 `?overwrite=true`），走同一套解析与校验，返回 200 `{valid, errors, warnings}`，`errors`/`warnings` 的每项是 `{code, params, error}`（`code` 稳定、`error` 是英文文案、`params` 含 `model_id` 与 `spec`）；解析或校验失败时 `valid=false`，`errors` 带 import 会返回的同一个 `code`（没有专属码的解析错误为 `CATALOG_INVALID`，`params.detail` 是原文）。不查「同名用户档位已存在」（import 返回 409）、不取权重大小、不写 store 与目录包存储。警告码见 [推理目录语法参考](inference-catalog-spec.md#导入前检查与警告) | JWT(admin) |
| POST | `/api/v1/inference-templates/import` | 导入模板/目录包；成功返回 `{models, package_digest}`，有警告时多一个 `warnings`（同 `evaluate`，无警告时不出现该字段） | JWT(admin) |
| POST | `/api/v1/inference-templates/sync` | 「检查更新」：从发布通道取官方推理模板目录包，按 upstream 来源对账（用户档位不动）；请求体忽略。返回 `{models, package_digest, version}`；目录包 schema 高于本 server 时 409 `UPDATE_CATALOG_INCOMPATIBLE`；目录包解析或校验不过时 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER`（`params.error` 带原始报错，界面提示该版本推理模板依赖最新版本服务端）；其余错误同 `/images/sync` | JWT(admin) |
| POST | `/api/v1/inference-templates/official` | 上传官方推理模板目录包（请求体为 `.tar.gz` 目录包，上限 32 MiB），与「检查更新」走同一条 upstream 对账；不是目录包时 400 `OFFICIAL_CATALOG_PACKAGE_REQUIRED`，解析或校验不过时 400 `OFFICIAL_CATALOG_NEEDS_NEWER_SERVER` | JWT(admin) |

推理模板包 schema（原始模型 + 可执行 spec 列表）见 ADR 0009/0010；对应参考文档
`inference-catalog-package.md`（如已发布）。

## Deployments / Instances（部署与实例）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/deployments` | 部署列表；带 `page`/`page_size` 时分页返回列表列并可按 `q`/`state` 筛选（见“错误与响应约定”） | JWT |
| GET | `/api/v1/routing-policies` | 服务端下发的多副本路由策略及本地化说明 | JWT |
| POST | `/api/v1/deployments/preflight` | 多机推理部署前置检查（rank 分配、网络探测） | JWT |
| POST | `/api/v1/deployments` | 创建部署（`model_source` 只有 `{storage_id, provider}`：保存模型的存储与运行哪家权重来源，不接受镜像获取方式；`placement.pd` 为 PD 分离部署，见下）。`served_name` 已被另一个未在删除的部署占用时 409 `SERVED_NAME_IN_USE`（`params.served_name`、`deployment`、`deployment_id`），不写时取模型名、同样校验 | JWT |
| GET | `/api/v1/deployments/{id}/candidates` | 部署候选池的实时情况：按「下一个副本」的视角把集群每台节点归到它没通过的第一道检查（`room` 看上去有空卡、`busy` 卡被醒着的实例占用、`unmet` 不满足节点条件、`cordoned` 已停止调度或驱逐中、`offline` 离线），`counts` 覆盖全部节点、相加等于 `nodes`，`unmet_by_key` 按首个不满足的条件键计数；`items` 是一页明细（候选按副本尝试顺序带 `rank`，其后是不满足的与离线的），`?status=` 过滤，`?offset=`、`?limit=`（1–200，默认 50）分页，参数不合法 400 `FIELD_INVALID`。只做批量读、不做试算：「看上去有空卡」的是否真放得下以控制器的试算为准，见部署详情的 `placement_status` | JWT |
| PUT | `/api/v1/deployments/{id}/requirements` | 不重新部署，替换普通单机部署的节点条件与放置策略：`{"node_requirements": {...}, "placement_strategy": "spread"}`（策略不写则保持）。只影响此后启动的副本，运行中的副本不迁移（重启副本即按新条件重新放置）。厂商由部署镜像决定：不写则保留，写成别的厂商 400 `NODE_REQUIREMENTS_INVALID`；条件校验同创建；与创建一样做「永远放不下」检查，新条件下没有一台节点空载时放得下一个副本则 400 `DEPLOYMENT_NEVER_FITS`，原条件不变。多机与 PD 部署逐 rank 列节点，400 `NODE_REQUIREMENTS_NOT_EDITABLE`（`params.shape` 为 `multi_node` 或 `pd`）；删除中 409 `DEPLOYMENT_DELETING`；成功返回部署详情 | JWT |
| GET | `/api/v1/deployments/{id}` | 部署详情。普通单机部署有副本还没有实例时带 `placement_status{unscheduled, reason, at}`：等待节点的副本数，以及控制器最近一次记下的原因（`pool_empty` 或 `pool_no_fit`，参数里有各类计数与最接近的 3 台候选及其拒绝原因；每个等待超时窗口最多记一次）。`node_requirements` 是部署的节点条件（总是出现，没有时为 `{}`；钉了节点的部署在 `gpuworks.ai/node` 下列出它们，已删除节点的值仍保留、不再匹配任何节点），`node_requirement_labels[]` 把其中每个值按节点页的显示方式给出（`{key, value, display, display_code?, nodes}`：节点与网络显示名称，`nodes` 是此刻带这个值的节点数，0 表示已删除的节点或网络、没有节点带的标签——它仍然约束部署，只是匹配不到任何节点；只有单个部署的读取带这个字段，列表不带），`placement_strategy` 是 `spread` 或 `binpack`；`ready_replicas` 是此刻所有 rank 都在运行的副本数（PD 部署不带，看 `pd` 的各角色）；`health` 是部署的健康检查（见下「健康检查」）；`tokenize_enabled` 说网关是否把 `/v1/tokenize`、`/v1/detokenize` 转给它（见 [OpenAI 兼容网关](openai-gateway.md#分词tokenize)）；`max_replicas` 是副本数上限（改副本时的校验边界）；`restart_requested_at` 是进行中的滚动重启的请求时间（没有时缺省）；普通部署的 `retry_exhausted{index,failed_attempts,last_failure}` 指出用完重试次数、控制器已放弃重建的副本（用 `units/retry` 重试）；多机部署列出副本时带 `units[]`（每个副本的 leader 与 `followers[]`） | JWT |
| PUT | `/api/v1/deployments/{id}/routing` | 修改路由策略（`least_request`、`prefix_affinity` 或 `failover`） | JWT + 管理员 |
| PUT | `/api/v1/deployments/{id}/dgpu` | 改单机 dGPU 部署的显存上限，立即生效：`{"vram_limit_gib": 12}`。部署之后的实例按新上限启动；运行中的实例当场改上限（实例期望状态里的 `vram_limit_bytes` 与 HAMi 共享区同步改写，不重启），除非后端只在启动时读上限，或调低时实例占用超过新上限、正在启动或休眠、没有用量上报——这些实例重启后生效。返回 200 `{"deployment": {...}, "live": [实例 UUID], "pending": [{"instance_uuid", "reason": "backend"\|"in_use"\|"starting"\|"asleep"\|"no_usage", "used_bytes"}]}`。拒绝：整卡部署 400 `DGPU_QUOTA_NOT_PARTITIONED`；多机/PD 400 `DGPU_QUOTA_NOT_EDITABLE`（`params.shape`）；上限非正 400 `DGPU_QUOTA_INVALID`；新上限下没有节点能放下 400 `DEPLOYMENT_NEVER_FITS`；调高时实例所在卡在扣掉自身占用后放不下 409 `DGPU_QUOTA_NO_ROOM`（`params.node`、`gpu`、`free_gib`、`need_gib`）；删除中 409 `DEPLOYMENT_DELETING`。时间线记 `DEPLOYMENT_DGPU_QUOTA_CHANGED`（`from_bytes`、`to_bytes`、`live`、`pending`） | JWT |
| PUT | `/api/v1/deployments/{id}/health-check` | 开关部署的健康检查并设阈值：`{"enabled": true, "threshold_sec": 60}`（`threshold_sec` 1–600，省略或 0 保持当前）。开启要求模型提供 chat 或 embedding，否则 400 `FIELD_INVALID`（`params.field` 为 `health_check`）；阈值越界 400 `FIELD_INVALID`；不重启实例，下一轮检查即按新设置。返回部署详情 | JWT |
| PUT | `/api/v1/deployments/{id}/tokenize` | 开关网关对该部署的分词转发（`/v1/tokenize`、`/v1/detokenize`）：`{"enabled": false}`。`enabled` 必填，缺省 400 `FIELD_INVALID`（`params.field` 为 `enabled`），不是布尔 400；部署不存在 404。不重启实例，网关路由表随变更事件重建后生效。返回部署详情（带 `tokenize_enabled`） | JWT |
| GET | `/api/v1/deployments/{id}/events` | 部署事件时间线（最新优先、游标分页、读取时折叠） | JWT |
| POST | `/api/v1/deployments/{id}/clone` | 克隆部署配置，节点条件与放置策略一并复制。副本的对外模型名为原名加 `-` 和 4 位随机小写字母或数字；请求体 `{"name": ...}` 同时设定部署名与对外模型名，被占用时 409 `SERVED_NAME_IN_USE` | JWT |
| POST | `/api/v1/deployments/{id}/stop` | 停止部署：删除全部实例，保留配置 | JWT |
| POST | `/api/v1/deployments/{id}/start` | 启动已停止的部署，按保留的配置重新调度（每个副本的重试次数重新计）；已在运行则不做任何事；删除中的部署 409 `DEPLOYMENT_DELETING` | JWT |
| DELETE | `/api/v1/deployments/{id}` | 删除部署 | JWT |
| POST | `/api/v1/deployments/{id}/sleep` | 休眠部署（`?level=1\|2`，默认 1；`?mode=wait\|abort`，默认 wait）：睡眠意图写进每个活跃实例的期望状态，网关立即不再派发给它们，部署记为"整体处于睡眠命令下"（此后新起的实例也按此睡眠）。`?role=prefill` 只睡 PD 部署该角色的实例（响应带 `role`、`instances`），部署整体不算在睡眠命令下；非 PD 400 `PD_NOT_PD`，不是该部署模式的角色 400 `PD_ROLE_UNKNOWN`，模式不允许为空的角色（如 decode）400 `SLEEP_ROLE_CANNOT_EMPTY` | JWT |
| POST | `/api/v1/deployments/{id}/wake` | 唤醒部署：已睡着的实例须放得回原卡，否则 409 `WAKE_INSUFFICIENT_GPU_MEMORY`（全有或全无）。`?role=` 只唤醒 PD 该角色的实例。`?partial=true` 唤醒放得下的那部分，响应 `{"status":"waking","woken":[...],"refused":[{"instances","node","gpu_indexes","shortfall_gib","occupants"}]}`；一个也放不下时仍 409。409 的参数描述第一个被拒的节点，`refused` 列出全部被拒的副本；任何唤醒都会撤销部署整体的睡眠命令 | JWT |
| POST | `/api/v1/deployments/{id}/units/suggest` | 为多机部署再推荐一个副本的节点，由用户在「调整副本」里确认后随列表保存（不写任何东西）：请求体 `{"units": [...]}` 是编辑器里已列出的副本（它们的 leader 不能再当 leader；部署还没有的副本按每 rank 卡数占用这些节点）。推荐的节点共享同一计算网络与 GPU 架构、各有一个 rank 的空闲卡、可调度且在线、满足部署节点条件，每个 rank 都按部署已保存的配置试算；最多看最好的 3 组、每 rank 试 3 台。返回 `{"unit": {"node_uuid", "followers"}, "network": {"id","name"}, "arch", "nodes": [{"uuid","name","free_gpus","model"}]}`；放不下时 `unit` 为 `null`，`reason.code` 为 `unit_none`（各类被排除的节点数与最大可组台数）或 `unit_no_fit`（第一台被拒节点及原因）。非多机部署 400 `REPLICAS_BODY_SHAPE` | JWT |
| POST | `/api/v1/deployments/{id}/units/retry` | 让一个副本重新获得重试次数：请求体 `{"role","index"}`（普通部署 `role` 为空），该副本的失败尝试被移除（容器清理、时间线保留），控制器随后重建它，其他副本不受影响；没有该副本 404 `UNIT_UNKNOWN`，没有失败尝试或仍有实例 409 `UNIT_NOT_FAILED`，部署非运行中 409 `UNIT_RETRY_NOT_ACTIVE` | JWT |
| PUT | `/api/v1/deployments/{id}/replicas` | 普通部署改副本。多机部署列出期望的全部副本 `{"units":[{"node_uuid":"<leader>","followers":[{"node_uuid":"..."}]}]}`：每个副本恰好 `nnodes` 个 rank、各在不同的已知节点（`REPLICA_UNIT_RANKS` / `REPLICA_UNIT_NODE`），不指定 GPU（`REPLICA_UNIT_GPUS`），不同副本的 leader 不在同一台节点（`REPLICA_LEADER_SHARED`，leader 监听部署的 master 端口），每台节点上列出的 rank（不论属于哪个副本）所需 GPU 不超过它的卡数（`REPLICA_UNIT_GPUS_INSUFFICIENT`）；新增或换了节点的副本还须在线（`REPLICA_UNIT_NODE_OFFLINE`）并满足部署的节点条件（`REPLICA_UNIT_REQUIREMENTS`），部署已列出、节点与顺序都不变的副本原样保留，不因节点掉线或标签变化而挡住整个列表的修改；换了首个副本的 leader 时，部署上存的 master 地址随之清空，每个副本在自己 leader 的参数网络地址上汇合；列表里去掉的副本排空后退役，新副本各自成组，在自己的 leader 上汇合；响应的 `units` 列出当前副本及其重试情况。单机部署改副本数：`{"replicas":N,"remove":["<uuid>"]}`，`remove` 可选，指定缩容时移除哪些实例的副本（进入 `draining`，处理完请求后退役）；不指定时由服务端选（先未就绪的，再最新创建的）；1 ≤ N ≤ 部署的 `max_replicas`（当前 32；`REPLICAS_TOO_FEW` / `REPLICAS_TOO_MANY`）；`remove` 多于减少的副本数 400 `REPLICAS_REMOVE_TOO_MANY`，指定的实例没有在承担本部署的副本 400 `REPLICAS_REMOVE_INVALID`；请求体与部署形态不符（PD、多机传数量、单机传列表）400 `REPLICAS_BODY_SHAPE`；副本数或指定的实例在请求期间已被改动（并发扩缩、实例刚失败）409 `REPLICAS_CHANGED`，不写入任何改动；删除中 409 `DEPLOYMENT_DELETING` | JWT |
| PUT | `/api/v1/deployments/{id}/pd` | PD 部署调整各角色实例（`roles.<role>.units`）与 `fallback`；`mode`、角色的 `tp`/`pp`、`kv_load_failure` 不可变（`PD_IMMUTABLE`）；非 PD 部署 400 `PD_NOT_PD` | JWT |
| POST | `/api/v1/deployments/{id}/restart` | 重启部署，适用于所有形态，按当前网络与镜像解析重建，每个副本的重试次数重新计（失败的尝试一并退役）。可选请求体 `{"strategy":"rolling"\|"all"}`：默认 `rolling`，逐个替换副本（副本先 `draining` 处理完手上的请求，同时启动替换实例；普通多机副本的每一代都在 leader 上监听同一个 master 端口，替换实例等旧一代退出后才在同一台节点上启动；全部副本都恢复服务后才换下一个；进行中时部署响应带 `restart_requested_at`），记 `DEPLOYMENT_ROLLING_RESTARTED{replicas}`；`all` 立即停掉全部实例后一起重建，期间服务中断，记 `DEPLOYMENT_RESTARTED{instances}`。未知 strategy 400 `RESTART_STRATEGY_INVALID`；非 active 部署 409 `DEPLOYMENT_NOT_ACTIVE` | JWT |
| POST | `/api/v1/evaluate` | 显存估算、调度试算与获取预估（不创建部署）：响应的 `acquisition` 含 `present`（已持有模型、目标节点读得到的存储）、`targets`（节点取模型时可保存到的存储）、`providers`（模板的权重来源）、`image`，以及请求指定了节点时的 `nodes[]`（见「评估请求与获取预估」） | JWT |
| GET | `/api/v1/instances` | 实例列表 | JWT |
| GET | `/api/v1/instances/{uuid}` | 实例详情。服务端推导的字段：`replica`（该实例承担的副本 `{role,index}`，不承担时缺省）、`asleep`（因睡眠不接收请求：已下令睡眠或仍在睡）、`desired_sleep`（下令的级别）、`can_sleep_alone` / `can_wake_alone`（能否单独睡、单独醒）；多机副本的 follower 不上报自己的睡眠级别，`sleep_level` 给的是它所在代 leader 的级别；`dist_role` 对未记录角色的旧行按 headless 与 rank 推出。部署详情里的 `instances[]` 带同样的字段，PD 角色带 `may_be_empty` | JWT |
| GET | `/api/v1/instances/{uuid}/events` | 实例事件时间线（最新优先、游标分页、读取时折叠） | JWT |
| GET | `/api/v1/instances/{uuid}/launch-facts` | 实例启动事实三层投影（intent/resolved/runtime observation，unknown 不降级，敏感文本脱敏） | JWT |
| GET | `/api/v1/instances/{uuid}/logs` | 实例容器日志（SSE，经 agent logtail 隧道） | JWT |
| POST | `/api/v1/instances/{uuid}/sleep` | 只睡该实例所在的副本（`?level=`、`?mode=` 同部署级）；多机副本的任一 rank 都代表整个副本，意图写在它的全部实例上，由 leader 的引擎带着 follower 一起睡，响应的 `instances` 列出全部 rank。部署其余副本照常服务，部署不算整体睡眠（该副本被重建时新实例醒着）。部署不可睡 400（同部署级的码）；实例没有在承担副本 409 `INSTANCE_NOT_SERVING`；PD 模式不允许为空的角色的最后一个醒着的副本 409 `SLEEP_ROLE_CANNOT_EMPTY` | JWT |
| POST | `/api/v1/instances/{uuid}/wake` | 唤醒该实例所在的副本（多机副本同上，按整个副本），只检查这个副本放不放得回原卡（409 `WAKE_INSUFFICIENT_GPU_MEMORY`）；部署整体的睡眠命令随之撤销，其余睡着的副本保持睡眠 | JWT |
| POST | `/api/v1/instances/{uuid}/restart` | 重启该实例所在的副本（follower 代表它所在的整代）：默认让副本的 leader 进入 `draining`（网关不再派发新请求，处理完手上的请求后退役），同时启动替换实例；`?drain=false` 立即移除整代；不占用副本的重试次数；记部署事件 `REPLICA_RESTARTED{instance,drain}`；部署非 active 409 `DEPLOYMENT_NOT_ACTIVE`，实例没有在承担副本（失败、排空中、移除中）409 `INSTANCE_NOT_SERVING` | JWT |

`POST /evaluate` 与 `POST /deployments` 共用部署配置请求；评估结果不预留资源。新增配置字段如下：

| 字段 | 语义 |
|---|---|
| `quick_params` | 按 `/engines` 参数字典提交 typed 值；显式 `false` 与省略不同 |
| `quick_params_unset` | 本次明确不继承的参数 key 数组，阻止模板回填；不写入持久化配置 |
| `placement_mode` | `auto` 由服务端选择节点，`manual` 使用显式节点；省略按 `manual`。只是请求里的说法：部署不存这个字段，手动放置存为节点条件 `gpuworks.ai/node: [node_uuid]`（单机、非 PD） |
| `target_vendor` | 评估与创建使用同一加速器厂商；存为节点条件 `gpuworks.ai/vendor`。未给时创建按落点节点的厂商补上 |
| `node_requirements` | 节点条件：`{"<key>": ["<value>", …]}`，同一键的多个值是「其中之一」，不同键「同时满足」。键可以是节点标签，也可以是系统标签（见上「节点标签」）；保留域里不是这八个系统键的一律拒绝（拼错的键不会悄悄匹配 0 台），`gpuworks.ai/vendor` 只能有一个值，网络键的值是网络 ID 或 `management`。多机与 PD 部署逐 rank 列节点，不接受 `gpuworks.ai/node`。错误 `NODE_REQUIREMENTS_INVALID`；所选节点不满足 `NODE_REQUIREMENTS_UNMET`（`params.detail` 写明哪个键） |
| `placement_strategy` | `spread`（默认）或 `binpack`，随部署保存；其它值 `PLACEMENT_STRATEGY_INVALID` |
| `placement.gpus`、`placement.tp`、`placement.pp` | 明确的物理 GPU 索引；TP、PP 是整个部署的并行度（每节点 TP × PP ÷ `nnodes` 张卡，必须整除）。多机未给 TP 时默认每节点一个流水线阶段、TP 取所选节点中最少的 GPU 数，生效值写回 placement；单机未给 TP 时由调度器按 PP 的整数倍挑卡（TP = 卡数 ÷ PP），自动放置同样按所填 TP、PP 选节点；评估在 `form_values.tp/pp` 与 `parallelism`（`tp`、`pp`、`nodes`、`gpus_per_node`）中返回；多机评估与手动放置的 `acquisition.nodes` 逐节点给出获取预估 |
| `routing_policy` | 多副本网关策略：`least_request`（默认）、`prefix_affinity` 或 `failover`；不属于引擎启动参数 |
| `tokenize_enabled` | 网关是否把 `/v1/tokenize`、`/v1/detokenize` 转给该部署（见 [OpenAI 兼容网关](openai-gateway.md#分词tokenize)）：省略为开启，`false` 关闭；与引擎无关，引擎不提供时由引擎自己回错 |
| `health_check`、`health_check_threshold_sec` | 服务的健康检查（见下「健康检查」）：`health_check` 省略时，模型提供 chat 或 embedding 就开启，否则关闭；显式 `true` 而模型两者都不提供时创建回 400 `FIELD_INVALID`。阈值 1–600 秒，省略或 0 为 60，越界时评估给字段错误、创建回 400，码均为 `FIELD_INVALID` |
| `start_timeout_sec` | 本部署实例的启动健康检查预算（秒）：实例启动后等引擎健康检查通过的最长时间。省略或 0 表示跟随集群默认（设置 `deployment.start_timeout_sec`，出厂 1800）；否则须在 60–86400，越界时评估给字段错误、创建回 400，码均为 `FIELD_INVALID`。随部署保存，部署详情里有值时返回同名字段；值在实例创建时定下，修改只影响之后启动的实例 |

**健康检查。** 开启健康检查的部署，服务端每分钟经网关给它发一个最小请求：chat 发一条内容为 `1` 的用户消息、`max_tokens: 1`，只提供 embedding 的发输入 `1`；按网关的路由选副本，等到阈值为止。检查只报告，不重启、不摘除副本；网关不把它算作流量（不计请求数、token、TTFT/TPOT，不留请求记录，不刷新最近请求时间）；引擎自己导出的指标（如 vLLM 的请求计数）会计入这每分钟一次、1 个 token 的请求，网关无法替引擎扣除。没有醒着在运行的副本（已停止、全部休眠或还在启动）时不发。结果只在服务端内存里保留每个部署最近 10 次，重启服务端后一分钟内重新有结果。部署详情、列表行与监控服务页的 `health`：

| 字段 | 语义 |
|---|---|
| `status` | `ok`（阈值内回复）、`timeout`（阈值内没回复）、`failed`（回复了错误，或网关连不上副本）、`off`（未开启）、`no_replica`（没有可检查的副本，未发送）、`pending`（已开启、还没检查过） |
| `threshold_sec` | 阈值 |
| `last` | 最近一次检查 `{at_ms, status, latency_ms, http_status?, instance_uuid?, message?}`：`instance_uuid` 是回复它的副本，`message` 是错误回复的开头或没有回复的原因；超时的 `latency_ms` 记为阈值；没有时为 `null` |
| `recent` | 最近至多 10 次检查，旧的在前 |
| `recent_mean_ms`、`recent_ok` | 其中阈值内回复的平均耗时与次数；没有时为 `null`、`0` |

**评估请求与获取预估。** 部署请求不选择镜像或模型从哪里来（[ADR 0048](../adr/0048-pull-settings.md)）：`model_source` 只有 `{storage_id, provider}`，前者是节点取模型时保存到的存储，后者是运行模板的哪家权重来源（`huggingface` / `modelscope`）。`acquisition.nodes[]` 对请求指定的节点（手动放置或多机，按 rank 顺序）给出预估，只是预估，节点启动时会重新判断：

| 字段 | 语义 |
|---|---|
| `image_plan` | `present` 节点已有；`copy` 同步组内的 `image_copy_from` 节点复制；`pull` 从 `image_pull_refs` 拉取：前 `image_pull_fixed` 个是来源规则的替代地址，按顺序先试，其余是镜像自己的副本（`image_pull_ranked` 为真时由节点按探测延迟决定先后）；规则让该节点无处可拉时带 `image_pull_error`（`{code, params, message}`，code 一般为 `IMAGE_SOURCE_FORBIDDEN`，params 含 `node`、`route`） |
| `weights_plan` | `present` 已有；`copy` 从 `weights_copy_from` 节点复制；`storage` 从共享存储 `weights_storage` 读取或缓存；`download` 从 `weights_provider` 下载 |
| `image_cached`、`image_cache_reason`、`weights_present` | 镜像是否已缓存（未知时带原因）与模型是否已在节点上 |

自动放置时不逐节点预估，调度后由选中的节点自己获取。

**评估的字段错误（`errors[]`）与 `ok=false` 语义。** `POST /evaluate` 是交互端点：
可归因到请求字段的校验失败不再返回 400，而是 HTTP 200、`ok=false`，错误进入响应的
`errors[]`（每项 `{field, code, params?}`，`code` 是稳定 ProductError 码、与 create
对应 400 路径的错误码一一对应，`params` 为客户端 locale 插值参数）。表单保留草稿并把
错误显示在对应控件旁；`POST /deployments`（create）保持 400/409 不变。无法归因到字段的
失败（鉴权、模型不存在、服务端错误）仍按原状态码。字段路径约定：

- 快捷参数直接使用字典 key（即表单字段名），如 `max_model_len`、`gpu_memory_utilization`；
- 请求级字段使用 wire 名：`replicas`、`nnodes`、`engine`、`target_vendor`、`extra_args`、
  `quick_params_unset`、`master_port`、`placement`、`placement.tp`、`placement.pp`、`node_requirements`、
  `placement_strategy`、`partition_backend`、`start_timeout_sec`、`health_check_threshold_sec`；
- `node_uuids`（多机节点列表）在表单里对应 `distributed_nodes` 控件；
- 额外挂载行统一归到 `container_mount_rows`（请求只携带用户新增的挂载，不含行号）；
- docker 参数错误归到 `container_docker_args`，共享内存大小错误归到 `container_shm_gib`；
- **`field` 为空串的错误不归属任何控件**：启动装配/放置试算等与单个参数无关的失败
  （如节点没有该 GPU 序号、没有可用运行时镜像）以此承载，客户端应将其合并进页面级
  提示，不得挂在参数控件旁。

涉及的规则：副本形状（`REPLICAS_NEGATIVE`/`REPLICAS_TOO_MANY`/`DISTRIBUTED_REPLICA_COUNT`）、
节点条件（`NODE_REQUIREMENTS_INVALID`/`NODE_REQUIREMENTS_UNMET`）、放置策略（`PLACEMENT_STRATEGY_INVALID`）、
厂商（`TARGET_VENDOR_INVALID`）、引擎（`ENGINE_INVALID`）、
extra_args 解析（`EXTRA_ARGS_INVALID`）、挂载（`MOUNT_CONTAINER_PATH`/`MOUNT_HOST_PATH`/
`MOUNT_TARGET_DUPLICATE`/`MOUNT_TARGET_NESTED`）、json 型参数（`QUICK_PARAM_JSON_INVALID`）、
容器路径型参数（`QUICK_PARAM_PATH_INVALID`）、docker 参数与平台托管项或正式字段重复
（`DOCKER_ARG_PLATFORM_MANAGED`/`DOCKER_ARG_USE_SHM_FIELD`/`DOCKER_ARG_USE_ENV_FIELD`/
`DOCKER_ARG_USE_MOUNT_FIELD`/`DOCKER_ARG_USE_ENTRYPOINT_FIELD`）、共享内存大小与 `--ipc=host` 同时设置
（`SHM_SIZE_WITH_HOST_IPC`）、共享内存大小小于 CPU 内存层（`SHM_BELOW_KV_CPU_TIER`）、参数冲突（`LAUNCH_CONFIG_CONFLICT`/
`LAUNCH_CONFIG_UNSET_CONFLICT`）与数值界
（`FIELD_INVALID`，`detail` 为英文兜底文本）。响应另带 `schema_version`（当前 1），
供客户端 fixture 测试断言契约不变。

除 `errors[]` 外，评估响应还携带表单所需的全部服务端判断（均 `omitempty`，见
`ui/tests/fixtures/evaluate/*.json` 的字面样例）：

| 字段 | 语义 |
|---|---|
| `form_values` | 表单应显示的完整值：生效配置（含字典默认与模板投影）+ `Moved` 之后的 `extra_args` 文本；前端 `setFieldsValue` 直接消费，不再自行推导 |
| `defaults` | 表单原先硬编码的常量：`model_mount`（`/model`）、`master_port`（29500）、`engine`（默认引擎） |
| `eligible_nodes` | 每个节点按本草稿的节点条件（含厂商；手动选的那台节点本身不算条件，下拉正是用来选它的）/模板 GPU 谓词/在线状态判定，`eligible=false` 时 `reasons[]` 给原因（`node_offline`、`node_requirements_unmet`、`vendor_mismatch`、`chip_mismatch`；节点条件本身无效时每个节点都是 `node_requirements_invalid`，不会全部显示为合格）；节点下拉只渲染此列表 |
| `weight_sources` | 模板 spec 声明的权重来源（`provider`/`repo_id`/`revision`/`subfolder`）+ `suggested`（服务端建议）与 `registered`（本集群已有登记行，按 repo id 身份判定）；选定后仍由客户端调注册 API |
| `preflight` | 仅 `nnodes>1`：请求级预检结论（节点数/重复/端口范围/在线/可路由地址）。深度网络探测仍在独立预检 RPC（CLI 保留），前端不再编排 |
| `eviction` | 当前显存不足但可通过休眠醒着的部署腾出空间时返回候选；每项含 `instance_uuid`、`deployment_id`、`deployment`、`gpu_indexes` 和 `frees_gib` |

字典默认值已归服务端：`Materialize` 对新请求套用 `ParamDefinition.Default`
（含 `DefaultByEngine`），空 `quick_params` 产出的 argv 与旧 UI 默认载荷逐字节相同
（`vllm_ui_default`/`sglang_ui_default` argv golden 锁定）。`quick_params_unset`
语义不变；依赖未启用的参数不套默认。`/engines` 字典新增 `layout`
（`quick`\|`expert`\|`hidden`，空串按 `expert`）与 `order`，快捷表单的布局由字典驱动。

`quick_params_unset` 只能包含当前引擎字典里的 key。若同一请求又在
`quick_params` 或 `extra_args` 为该 key 提供值，返回清除冲突；原有 `null`
语义保持不变，不能用 `null` 替代明确清除。复合引擎参数只有在适配器能无损识别、
且完整关闭对应配置时才能从模板移除；未知 connector 或混合配置返回冲突，避免误删其他能力。

自动模式评估返回 `selected_node`、逐节点 `candidates` 与选中节点的 `plan`。
创建时提交 `selected_node.uuid` 作为 `node_uuid`，同时提交 `plan.plan.gpus`
和 `plan.plan.tp`，并保留标签等原约束。创建会重新校验，原节点、GPU 或约束失效时
返回冲突，要求重新评估。当前自动确认只支持单节点、单副本；显式节点与旧请求的多副本
评估仍为 `plan_scope=per_replica`，不能据此推断整个副本组都能分配成功。

模板详情及评估响应中的 `engine_decides_param_keys` 是当前有效启动不替用户填默认值、
交给引擎决定的字典 key：启动入口或镜像里的引擎版本可能不认这个 flag（规则见
[启动配置](../explanation/launch-config.md)）。只列有字典默认值的 key；表单把它们显示为
「自动」，用户仍可显式打开或关闭，显式陈述的值照常发射，不会被拒绝。判断由引擎适配器做，
不能通过模板有没有写某个 flag 推断。新部署的用户入口覆盖优先于模板入口；手工命令不解释为
结构化引擎参数。

**（2026-09-22 起）引擎字典是"这个引擎能不能表达这个参数"的唯一判据。** 请求在
`quick_params` 里显式给某个 key 值，而所选引擎的字典没有这个 key 的 flag（或该 key 是
只对其它引擎生效的平台参数）时，`POST /evaluate`、`POST /deployments` 与克隆的配置差异
计算返回 400（`invalid quick_params: <key> is not supported by <engine>`），不再把该值
静默丢掉。唯一例外是该 key 的**中性默认值**——它与"没写"等价，例如 SGLang 的
`performance_mode=balanced`，此前接受、现在仍接受。

受影响的显式组合（此前静默忽略，现在 400）：

- SGLang：`enable_chunked_prefill`（两种极性）、`rope_scaling`、多模态 json 键
  （`limit_mm_per_prompt`、`mm_processor_kwargs`、`media_io_kwargs`、`allowed_local_media_path`）、
  `shared_kv_dir`、`kv_offload_read_threads`/`kv_offload_write_threads`、`kv_disk_limit_gib`、
  `async_scheduling=true`，以及 `performance_advanced` 组里所有 vLLM 专属键。
- vLLM：`log_level`（除 `info`，该引擎的日志级别由 env 承担）、`enable_metrics=true`、
  `speculative_num_steps`、`speculative_eagle_top_k`。
- callable：不发布 `param_specs`，除中性值外不接受任何字典参数。

UI 的默认载荷不受影响（`buildQuickParams` 按当前引擎字典构造）；脚本或第三方客户端把
vLLM 的载荷复用到 SGLang 上，会从"静默忽略"变成 400。`engine_decides_param_keys`
（按入口与引擎版本只撤默认值）与这条规则互补：前者不拒绝任何值，后者按**引擎**拒绝它表达不了的值。


### PD 分离部署（`placement.pd`）

```json
{"placement": {"pd": {
  "mode": "vllm-nixl",
  "roles": {
    "prefill": {"tp": 1, "units": [{"node_uuid": "n1", "gpus": [0]}]},
    "decode":  {"tp": 2, "pp": 1, "units": [{"node_uuid": "n2", "followers": [{"node_uuid": "n3"}]}]}
  },
  "kv_load_failure": "fail",
  "fallback": "decode"
}}}
```

- `mode`：引擎档位 `pd_modes` 中的一个（`vllm-nixl`、`vllm-ascend-mooncake`、`sglang-nixl`）；未知字段一律拒绝。
- 每个 `unit` 是一个实例；`gpus` 可省略。`followers` 使该实例跨多台节点（rank 1…N），此时角色必须写 `tp`，
  每个 rank 运行 `tp × pp ÷ rank 数` 张卡。
- `fallback` 与 `kv_load_failure` 的可选值和默认值按模式定：`vllm-nixl`、`vllm-ascend-mooncake` 的 `fallback`
  为 `decode`（默认）或 `fail`，`kv_load_failure` 为 `fail`（默认）或 `recompute`；`sglang-nixl` 的 decode 不能单独
  完成请求，`fallback` 只有 `fail`，也没有 `kv_load_failure`。模式不允许的值返回 `PD_FALLBACK_INVALID` /
  `PD_KV_LOAD_FAILURE_INVALID`。
- `GET /api/v1/engines` 每个引擎的 `pd_modes[]` 给出这些规则：`name`、`vendors`、`roles`、`scalable_to_zero`（运行中可缩到 0
  的角色）、`tp_rule`、`max_units_per_role`、`fallbacks`、`kv_load_failures`（首项为默认值）、`unverified`。
- 与 `nnodes`、`node_uuids`、`replicas > 1` 或自动放置同时给出时返回 `PD_SHAPE_CONFLICT`；其余校验错误码以
  `PD_` 开头（字段 `placement.pd`）。可休眠（`sleepable`）且 KV 经 UCX 走主机 RDMA 网络时，创建与评估返回
  `PD_SLEEP_RDMA_UNSUPPORTED`（`params`：`mode`、`network`）。
- 部署详情的 `pd` 字段：`mode`、`fallback`、`kv_load_failure`、`state`（`not_ready|partial|running`）、`short`、
  `roles[]`（`role`、`tp`、`pp`、`desired`、`ready`、`may_be_empty`（模式允许该角色为空，即可以单独休眠）、`units[]`，含 `followers[]`）；实例带 `role`。
  每个 unit 另有 `failed_attempts`、`last_failure` 与 `retry_exhausted`（控制器已放弃重建，可用上面的 retry 接口重试）。
- `POST /api/v1/evaluate` 的响应另有 `pd.roles[].units[]`：每个实例（及其 `followers[]`）的放置、地址、显存拆分与命令。

## Benchmarks（评测）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/benchmarks` | 评测任务列表 | JWT |
| GET | `/api/v1/benchmarks/meta` | 评测字典（数据集/指标元信息） | JWT |
| POST | `/api/v1/benchmarks` | 创建评测任务 | JWT |
| POST | `/api/v1/benchmarks/probe` | 探测外部评测目标（连通性/鉴权） | JWT |
| POST | `/api/v1/benchmarks/models` | 列出可评测模型 | JWT |
| POST | `/api/v1/benchmarks/datasets/import` | 导入评测数据集 | JWT |
| POST | `/api/v1/benchmarks/tokenizers/import` | 导入评测 tokenizer | JWT |
| GET | `/api/v1/benchmarks/{id}` | 评测任务详情 | JWT |
| GET | `/api/v1/benchmarks/{id}/curve` | 评测负载曲线数据 | JWT |
| GET | `/api/v1/benchmarks/{id}/report` | 下载评测报告 | JWT |
| DELETE | `/api/v1/benchmarks/{id}` | 删除评测任务 | JWT |
| GET | `/api/v1/benchmarks/{id}/points/{seq}/detail` | 下载某负载点的明细数据 | JWT |
| POST | `/api/v1/benchmarks/{id}/cancel` | 取消运行中的评测 | JWT |

## Profiles / GPUs（估算校准与 GPU 清单）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/gpus` | 集群 GPU 清单（含最新指标，服务端 20 秒快照）。不带分页参数时返回全部；带 `page`/`page_size` 时返回一页 `{items, page, page_size, total, overview, facets}`：`overview` 与 `facets{nodes[{uuid,name}], models, vendors}` 覆盖全部 GPU、不受筛选影响；可按 `q`（节点名/UUID、型号、GPU UUID、PCIe 地址子串）、`node`（节点 UUID）、`vendor`、`model`、`allocation_state`（`available`/`partial`/`allocated`/`unusable`/`unknown`）筛选（均逗号分隔）。`allocation_state` 说的是「这张卡现在被要求做什么」：节点没连上是 `unknown`（所有读数都是上次上报留下的）；`unusable` 指完全不接活，原因见卡自身的 `schedulable`/`health_status`/`ownership`/`foreign_processes`；其余按占用程度分。`overview.health` 是卡按健康等级的分布（`ok`/`warning`/`error`），和能不能调度是两件事：告警卡照样服务，被直通或被平台外进程占用的卡健康是 `ok`。`sort` 可选 `node`、`model`、`vendor`、`allocation_state`、`occupants`、`utilization`、`memory`、`temperature`、`power`、`fan`、`bdf`、`numa`（无读数的排在前） | JWT |
| GET | `/api/v1/dgpus` | dGPU 实例清单 | JWT |

节点非在线时，GPU 的 `allocation_state` 为 `unknown`、`available_bytes` 为 0，不计入可分配数量；硬件健康信息和已有分配仍保留。状态随上述 20 秒快照刷新。

## API Keys（网关密钥）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/apikeys` | API key 列表（不含明文），含已吊销的 key：已吊销的带 `revoked_at`（UTC 时间），未吊销的不出现该字段 | JWT |
| POST | `/api/v1/apikeys` | 创建 API key；`name` 去除首尾空白后不能为空，否则返回 400 `API_KEY_NAME_REQUIRED`；明文仅创建响应中返回一次 | JWT |
| DELETE | `/api/v1/apikeys/{prefix}` | 按前缀吊销 API key：立即不能再认证，记录保留（名字留给它的历史用量），前缀不复用；返回 `{"status":"revoked"}`。吊销未知或已吊销的 key 不报错、不改动；所有 key 都吊销后网关回到匿名访问（与没有 key 时相同） | JWT |

## Platform Tokens（平台 API 令牌）

平台 API 令牌代替登录会话调用管理 API 与 MCP，与上面的网关密钥（只用于 `/v1`）是两种东西。明文 `gwp_` 开头，只在创建响应里出现一次，服务端只存 SHA-256。每个用户只看得到、管得了自己的令牌，且只能在登录会话里操作。

名称去除首尾空白后必须为 1–128 个 UTF-8 字节（不是字符数），否则返回 400 `PLATFORM_TOKEN_NAME_INVALID`。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/platform-tokens` | 当前用户的令牌：`{items:[{id,name,prefix,scope,created_at,expires_at,last_used_at}], total}`；`last_used_at` 每分钟最多更新一次 | JWT（登录会话） |
| POST | `/api/v1/platform-tokens` | 创建令牌：`{name, scope?: read\|write（默认 read）, expires_in_days?: 0–3650（0 不过期）}`，返回 201 `{token, item}`；`scope` 非法回 400 `PLATFORM_TOKEN_SCOPE_INVALID` | JWT（登录会话） |
| DELETE | `/api/v1/platform-tokens/{id}` | 吊销（删除）自己的令牌，立即生效，返回 204；不存在或属于别人回 404 | JWT（登录会话） |

## Providers（云厂商租机）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/providers/kinds` | 支持的云厂商种类，含各自的凭据字段（`credential_fields[].default_value` 为可选的非敏感字段创建默认值，`placeholder` 仅为提示）、默认 `server_dial` 监听地址，以及 `server_dial_supported`（AutoDL 为 false：Pro 实例只开放 HTTP 端口）与 `server_dial_auto_port`（Vast、RunPod 为 true：驱动按平台的端口映射自选监听端口与接入地址） | JWT |
| GET | `/api/v1/providers` | 已配置的云厂商账号列表 | JWT |
| POST | `/api/v1/providers` | 新增云厂商账号 | JWT |
| GET | `/api/v1/providers/{id}` | 编辑回读：已保存的 endpoint/public_key 及字典声明的非 secret extra；不回填厂商默认值，不返回密钥。读取凭据失败返回 500，列表仍可用 | 登录会话（平台令牌及 AI/MCP 不可读） |
| PUT | `/api/v1/providers/{id}` | 更新云厂商账号 | JWT |
| DELETE | `/api/v1/providers/{id}` | 删除云厂商账号 | JWT |
| POST | `/api/v1/providers/{id}/sync` | 同步该账号下的实例：列出的实例写入/更新，云端已不存在的实例行移除（关联节点保留，带外恢复置 `stopped`）；列举失败返回 424、不改动实例行 | JWT |
| POST | `/api/v1/providers/{id}/instances` | 租用新实例；云厂商报错返回 424（不支持返回 501） | JWT |
| GET | `/api/v1/providers/{id}/instances` | 该账号下的实例列表 | JWT |
| DELETE | `/api/v1/providers/{id}/instances/{externalID}` | 释放/删除实例；云厂商确认后移除该实例行；云厂商报错返回 424（不支持返回 501），实例行不变 | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/power` | 开关机；云厂商报错返回 424（不支持返回 501） | JWT |
| POST | `/api/v1/providers/{id}/instances/{externalID}/resize` | 变更实例规格；云厂商报错返回 424（不支持返回 501） | JWT |
| POST | `/api/v1/providers/{id}/install` | 在已租实例上安装 agent；`agent_dial` 同样要求 `server_url`（`server_dial` 不需要），校验规则与错误码同上。`server_dial` 时：Vast 与 RunPod（供应商字典里 `server_dial_auto_port` 为 true）在 `server_dial_address` 留空时由驱动选实例上已映射且无人监听的 TCP 端口（给了 `server_dial_listen` 就优先用它的端口，它必须已被映射），并把监听端口与公网地址记到节点上；AutoDL（`server_dial_supported` 为 false）不支持 `server_dial`。RunPod 没有空闲的已暴露端口时返回 409 `INSTANCE_RESTART_REQUIRED`（尚未改动任何东西；`params.need` 说明为什么要重启：`port` 开放端口，`ssh_key` 给不认平台公钥的 Pod 加上公钥（`agent_dial` 也会遇到），`ssh_key_port` 两者都要），请求体带 `allow_instance_restart: true` 重试才会修改 Pod，这会重启 Pod 并重新编号全部端口。阿里云云助手不处理 `server_dial`，自动选择时改用 SSH；经 SSH 安装前，云助手在线时先把平台公钥加进 root 的 `authorized_keys`。`install_dir` 是实例上的安装目录，省略为 `/opt/gpuworks`；必须是 `/` 开头、不是根目录的绝对路径（不展开 `~`），否则在建节点前返回 400 `INSTALL_DIR_INVALID`（`dir`）；填了的目录写不进时安装失败，不回退到 `/tmp/gpuworks`；记为节点的安装目录，带外重启与重装都在这里进行。`no_systemd: true` 让 agent 以 nohup 运行、不注册 `gpuworks-agent.service`（同 `/api/v1/nodes/install`）。安装失败（SSH 登录不上、实例未授权平台公钥等）返回 424 `{error}`，正文是需要操作者处理的原因 | JWT |

云厂商相关接口里，云厂商 API 或实例 SSH 出错一律返回 424 Failed Dependency，正文 `{error}` 是原因。不用 502/504：Cloudflare 等 CDN/隧道会把源站 502/504 的正文换成它自己的错误页，原因就丢了。

## Web Terminal

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/nodes/{uuid}/terminal` | 打开 Web 终端会话（WS upgrade；host 或 container target） | JWT(admin) |
| GET | `/api/v1/nodes/{uuid}/terminal/settings` | 该节点终端能力与限制（kill switch、并发上限等） | JWT(admin) |
| GET | `/api/v1/terminal/sessions` | 终端会话审计记录（仅元数据，不留终端内容） | JWT(admin) |

## Networks

见[配置节点网络](../how-to/configure-networks.md)。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/networks` | 网络列表（含加载关系、解析节点数、有提示的节点数）与角色/类型字典（`types`：`host_ethernet` / `host_rdma` / `accelerator_rdma`；`type_roles`：每种类型可承担的角色）；第一项是管理网（`id` 0、`builtin: true`） | JWT |
| POST | `/api/v1/networks` | 新建网络（名称、角色、类型、CIDR、网卡名通配）。加速器 RDMA 只能承担参数角色，否则返回 `NETWORK_TYPE_ROLE_INVALID` | JWT(admin) |
| GET | `/api/v1/networks/{id}` | 网络详情：加载关系、解析到的节点（网卡/IP/HCA/提示，以及节点的用户标签 `labels`）、最近一次内部测试；`id` 0 是管理网，节点带 `roles_off`；不能修改、删除、加载（返回 404），可以发起内部测试 | JWT |
| PATCH | `/api/v1/nodes/management-roles` | 批量修改管理网角色：`{"node_uuids":[...],"allow":[...],"deny":[...]}`，只改列出的角色，所有节点同一事务，返回 `{"updated":N}`（实际有变化的节点数）；节点不存在返回 404 `MANAGEMENT_ROLES_NODE_NOT_FOUND` | JWT(admin) |
| PUT | `/api/v1/nodes/{uuid}/management-roles` | 设置该节点管理网不承担的角色（`{"roles_off":[...]}`），返回该节点的网络视图；只对新实例生效 | JWT(admin) |
| PUT | `/api/v1/networks/{id}` | 修改网络 | JWT(admin) |
| DELETE | `/api/v1/networks/{id}` | 删除网络及其加载关系与测试记录（运行中的实例不受影响） | JWT(admin) |
| POST | `/api/v1/networks/{id}/attachments` | 加载到一个节点（可带网卡/IP/HCA 覆盖）或一个节点标签 | JWT(admin) |
| PUT | `/api/v1/networks/{id}/attachments/{attachmentID}` | 修改节点加载的覆盖值 | JWT(admin) |
| DELETE | `/api/v1/networks/{id}/attachments/{attachmentID}` | 移除加载 | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests/estimate` | 同一个请求体，不发起测试：返回解析出的节点数与节点对数；卡测试另返回 `card`（`rounds`、`probes`、最长耗时 `seconds`、上限 `max_seconds`）。发起会被拒绝的请求在这里以同样的错误被拒绝 | JWT(admin) |
| POST | `/api/v1/networks/{id}/tests` | 发起内部测试（`kind`：tcp / rdma_tool / card_ping / card_matrix；rdma_tool 需主机 RDMA 网络，card_ping（同号加速卡互 ping）与 card_matrix（全卡矩阵）需加速器 RDMA 网络，轮数 × 3 秒超过 10 分钟时返回 `NETWORK_TEST_TOO_LONG`；`scope`：all / nodes / label；`id` 0 测管理网，只支持 tcp），返回 202 与运行记录（`node_uuids` 是被测节点，发起时固定，运行中尚无 `pairs` 时已给出）；由 controller 执行 | JWT(admin) |

## Settings / Network Presets

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/updates` | 设置页「版本与更新」与页头更新提示：同时请求全部发布源、取第一个验签通过的索引（同一渠道一小时内复用上次结果，`?refresh=true` 立即重查），返回渠道 `channel` 与能否切换 `channel_editable`、是否自动检测 `auto_check`（`false` 时页面加载只返回上一次手动检查的结果，从未检查过则是没有 `sources` 也没有 `unavailable_reason` 的空答复；`?refresh=true` 仍会检查）、胜出源、答复了的各源 `{url, ok, latency_ms, error}`（没等到的较慢源不列出；界面不展示），以及 `server` / `image_catalog` / `template_catalog` 各自的 `current`、`latest`、`update_available`（模板另有 `compatible`，server 另有 `installable`、`self_upgrade`、`self_upgrade_blocker` 与 `install_dir`（服务端所在的安装目录，即 `server.yaml` 所在目录，没有时省略）；server 的 `update_available` 只在渠道里的版本比当前构建新时为真；两份目录的 `update_available` 要求目录与当前不同，且渠道里的发布不早于当前构建——更早的发布带的是更旧的目录）。没有源可用时仍是 200，`available=false`，`unavailable_reason` 说明原因：`unpublished`（有源答复该渠道没有索引——源可达、渠道为空，例如还没有正式版本时的 release）、`invalid`（有源返回了验签或解析不过的内容，常见于需要登录的网关拦截）、`unreachable`（没有源给出答复：网络、代理、DNS、超时或服务端错误） | JWT(admin) |
| POST | `/api/v1/updates/prefetch` | 登录页请 server 在后台开始检查更新：不需登录，总是 204 空响应、不透露任何结果；已有新鲜的检查（成功 1 小时、失败 5 分钟内）或已有检查在跑、或自动检测已关闭时什么也不做，所以无论调用多频繁，server 对外的检查次数不变 | 无 |
| PUT | `/api/v1/updates/channel` | 切换更新渠道 `{"channel":"release"|"nightly"}`：写入 `server.yaml` 的 `update_channel` 并立即生效，返回 `{channel}`；没有 server.yaml 时 409 `UPDATE_CHANNEL_READ_ONLY`，其他值 400 `UPDATE_CHANNEL_INVALID` | JWT(admin) |
| PUT | `/api/v1/updates/auto-check` | 开关自动检测新版本 `{"enabled":true|false}`：写入 `server.yaml` 的 `auto_check_updates`（开启时删除该键）并立即生效，返回 `{enabled}`；关闭后 `prefetch` 不再检查，也不拉取通知。自动检查附带安装标识与版本，节点数与 GPU 型号只在 `usage_report` 开启时附带（见[配置](configuration.md#镜像目录与镜像拉取)）。没有 server.yaml 时 409 `UPDATE_AUTO_CHECK_READ_ONLY`，缺少或不是布尔值时 400 `UPDATE_AUTO_CHECK_INVALID` | JWT(admin) |
| GET | `/api/v1/usage-report` | 用户体验改进计划状态 `{enabled, editable, last_day?, last_attempt?: {at, day, ok, error?}, next_due_at?}`：是否加入（`server.yaml` 的 `usage_report`）、能否在控制台修改、最近一份送达的报告覆盖的 UTC 日、本进程最近一次投递、下一份报告的预计时间（[用户体验改进计划](usage-report.md)） | JWT(admin) |
| PUT | `/api/v1/usage-report/enabled` | 加入或退出 `{"enabled":true|false}`：写入 `server.yaml` 的 `usage_report`（加入时删除该键）并立即生效，返回状态。没有 server.yaml 时 409 `USAGE_REPORT_READ_ONLY`，缺少或不是布尔值时 400 `USAGE_REPORT_INVALID` | JWT(admin) |
| GET | `/api/v1/usage-report/preview` | 下一份报告的原文（`application/json`，与实际发送的字节相同，不取走进程内计数）；未接入改进计划的服务端 404 `USAGE_REPORT_UNAVAILABLE` | JWT(admin) |
| GET | `/api/v1/notices` | 页头通知铃铛 `{notices: [{id, severity, kind, title?, body?, url?, published_at, read}], unread}`：发布方签名发布的、适用于本安装的通知（`kind` 为 `security` / `release` / `event`，`title`/`body` 按语言给出纯文本）；按紧急程度、再按时间排序，`read` 与 `unread` 按调用者计算 | JWT |
| POST | `/api/v1/notices/read` | 把通知标为已读 `{"ids": [...]}`（1–128 个，每个 ≤64 字节，否则 400 `NOTICE_IDS_INVALID`）；只记调用者自己的已读，重复标记无副作用，204 | JWT |
| POST | `/api/v1/updates/server` | 在控制台升级服务端：从发布通道下载本平台压缩包（签名索引校验），备份后替换二进制并重启，返回 202 `{from, to, backup}`；下一次启动确认或回滚（ADR 0040）。不能自我升级时 409 `SERVER_UPGRADE_UNAVAILABLE`（`reason`：`no_service_manager` / `not_installed` / `not_installed_binary` / `not_writable`），已是该版本 409 `SERVER_ALREADY_AT_RELEASE`，渠道里的版本比当前构建旧（例如从 nightly 切到 release）409 `SERVER_NEWER_THAN_RELEASE`，渠道没有本平台的包 404 `UPDATE_NOT_PUBLISHED`，索引里列出的压缩包没有一个源能按索引的大小与 sha256 提供 502 `UPDATE_DOWNLOAD_FAILED`（`file` 为该文件路径，`detail` 列出每个源的原因，不替换运行中的服务） | JWT(admin) |
| POST | `/api/v1/updates/server/upload` | 同上，但请求体是上传的发布压缩包（上限 512 MiB）；不是可用的发布包时 400 `SERVER_UPGRADE_ARCHIVE_INVALID`（含控制台升级之前的版本：它们不会确认或回滚自己的启动，`detail` 提示改用 `gpuworks upgrade --archive`） | JWT(admin) |
| GET | `/api/v1/settings` | 全部设置项当前值（不含 `telemetry_export` 与 `branding`，它们只经各自的专用接口读写） | JWT |
| PUT | `/api/v1/settings/{key}` | 修改单个设置项；`telemetry_export`、`branding` 回 400 `SETTING_HAS_OWN_ROUTE`。`deployment`（待调度超时、实例启动超时、事件保留）只有管理员可改（否则 403 `ADMINISTRATOR_REQUIRED`）：请求体叠加在当前值上，只改其中出现的键，叠加后越界回 400 `FIELD_INVALID` 且不写入 | JWT(admin) |
| GET | `/api/v1/security/settings` | 安全开关：`playground_preview_network`（试验场 HTML 预览是否允许发起网络请求，默认 `false`） | JWT |
| PATCH | `/api/v1/security/settings` | 修改上述开关，只改 body 里出现的字段 | admin |
| GET | `/api/v1/branding` | 品牌外观：`product_name`、`console_title`、`login_description`、`logo`（base64 `data:` URL，PNG/JPEG/WebP/SVG），四个字段总是出现，空串表示用内置默认值（默认文案由客户端按界面语言渲染）。登录页在登录前就要显示，所以不需登录 | 无 |
| PATCH | `/api/v1/branding` | 修改品牌外观，只改 body 里出现的字段，空串恢复该项默认；文字去掉首尾空白后 `product_name` 最多 32 字、`console_title` 最多 64 字、`login_description` 最多 200 字（只有它允许换行），不得含控制字符；`logo` 解码后最多 128 KiB。不合法时 400 `FIELD_INVALID`（`field`、`detail`）且不写入；返回保存后的完整值 | admin |
| GET | `/api/v1/telemetry/export` | 网关请求 trace 的 OTLP 导出设置与运行状态：`enabled`、`endpoint`（Collector 的 OTLP/HTTP 地址）、`headers`（只列 `{name}`，值从不返回）、`sample_ratio`（网关自起 trace 的采样比例，0..1，从未设置时为 1）、`trace_link`（含 `{trace_id}` 的外部追踪系统链接，可空）；`status`：`active`、`traces_url`（实际发往的地址）、自 server 启动以来的 `exported`/`dropped`（队列满丢弃）/`failed`（被拒或送达失败）、`queued`、`last_error` 与 `last_error_at`、`last_export_at`（Unix 毫秒，没有时 `null`）、`setting_error`（已存设置无法应用的原因，如解密失败） | JWT |
| PUT | `/api/v1/telemetry/export` | 整体保存上述设置并立即生效：`{enabled, endpoint, headers:[{name, value}], sample_ratio, trace_link}`；header 不给 `value`（或为 `null`）时沿用已存的同名值，没列出的已存 header 删除；`sample_ratio` 不给为 1。header 值加密存储。地址无效 400 `TELEMETRY_ENDPOINT_INVALID`，header 名非法、重复或是导出器自设的 400 `TELEMETRY_HEADER_INVALID`（`name`），新 header 没给值 400 `TELEMETRY_HEADER_VALUE_REQUIRED`（`name`），比例越界 400 `TELEMETRY_SAMPLE_RATIO_INVALID`，链接不是含 `{trace_id}` 的 http(s) 地址 400 `TELEMETRY_TRACE_LINK_INVALID`；未知字段 400 | JWT(admin) |
| POST | `/api/v1/telemetry/export/test` | 用请求体里的草稿设置（同 PUT，不保存、不要求 `enabled`）发一个名为 `gpuworks test span` 的测试 span，只发一次不重试：200 `{ok, trace_id, traces_url, error}`，`ok=false` 时 `error` 是 Collector 的答复或连接错误；草稿本身不合法时同 PUT 的 400 | JWT(admin) |
| GET | `/api/v1/pull/source-rules` | 来源规则列表（按生效顺序）：`{id, position, kind, match, endpoints[], allow_upstream, control_plane, note, node_requirements, matched_nodes}`；`kind` 由 `match` 推出：带 `http://`/`https://` 的是站点规则 `url`（模型站点，如 `https://huggingface.co`），否则是镜像规则 `image`；`control_plane` 为管理节点取模型元数据时也按此规则找地址（只有站点规则可设） | JWT |
| POST | `/api/v1/pull/source-rules` | 新建来源规则；`allow_upstream` 必填，`control_plane` 默认 `false`。站点规则的 `match` 是站点根地址（不带路径），端点是 http(s) 地址、可带路径；规则不合法 400 `PULL_SETTING_INVALID`（`field`：`match`、`endpoints`、`control_plane`、`note`、`requirements`） | JWT(admin) |
| PUT | `/api/v1/pull/source-rules/{id}` | 整体替换一条规则 | JWT(admin) |
| PUT | `/api/v1/pull/source-rules/order` | 重排：`{"ids":[...]}` 列出全部规则各一次 | JWT(admin) |
| DELETE | `/api/v1/pull/source-rules/{id}` | 删除规则 | JWT(admin) |
| POST | `/api/v1/pull/source-rules/test` | 测试连通（不保存）：`{match, endpoints, allow_upstream, node_uuid, repo}`，站点规则草稿与一台节点（控制台对所选的 1–5 台节点并行各发一次）；`repo` 省略为 `Qwen/Qwen3-0.6B`。节点按真实下载的路径（节点代理、no_proxy、凭据）并行访问每个地址的确定接口（Hugging Face 类 `/api/models/<repo>/revision/main`，应答后再取 `/<repo>/resolve/main/config.json` 的第一个字节，门禁仓库只在文件上拒绝；`modelscope.cn` 为 `/api/v1/models/<repo>`），各 10 秒：`{source, repo, node_uuid, node_name, outcomes:[{base, status, code, latency_ms, error}]}`，`status` 为 `ok` / `auth_required`（401/403，需要凭据）/ `http_error` / `unreachable`。草稿不合法 400 `PULL_SETTING_INVALID`，节点不存在 404，离线 409 `NODE_OFFLINE`，节点无法执行（agent 早于 server 等）502 `SOURCE_PROBE_FAILED`（`detail`） | JWT(admin) |
| GET | `/api/v1/pull/sync-groups` | 同步组列表：`{id, name, node_requirements, rate_limit_kib_s, matched_nodes}` | JWT |
| POST | `/api/v1/pull/sync-groups` | 新建同步组（节点条件选择成员，限速可选） | JWT(admin) |
| PUT | `/api/v1/pull/sync-groups/{id}` | 整体替换同步组 | JWT(admin) |
| DELETE | `/api/v1/pull/sync-groups/{id}` | 删除同步组 | JWT(admin) |
| GET | `/api/v1/pull/metadata-sources` | `?source=` 取 `huggingface` 或 `modelscope`：控制台浏览器读取模型元数据的地址，即勾选了「用于管理节点获取模型元数据」的站点规则给出的候选（按顺序，最后是原站，除非规则关闭回源）：`{source, bases[]}`；规则禁止该站点所有地址时 `bases` 为空。其他 `source` 400 `FIELD_INVALID` | JWT |
| GET | `/api/v1/pull/proxies` | 代理条目列表（按匹配顺序，节点用首个选中它的条目）：`{id, position, http_proxy, https_proxy, no_proxy, username, has_password, control_plane, note, node_requirements, matched_nodes}`；地址不含登录信息，密码从不返回；`control_plane` 为管理节点出站请求也走此代理（取首个勾选的条目） | JWT |
| POST | `/api/v1/pull/proxies` | 新建代理条目：`{http_proxy, https_proxy, no_proxy, username, password, control_plane, note, node_requirements}`，至少一个代理地址；地址写成 scheme://host:port（`http`/`https`/`socks5`/`socks5h`，省略时为 `http`），登录信息写进地址 400；`no_proxy` 与 NO_PROXY 环境变量同一写法。密码加密存储。不合法 400 `PULL_SETTING_INVALID`；保存后在线节点各收到一次只含网络部分的更新 | JWT(admin) |
| PUT | `/api/v1/pull/proxies/{id}` | 整体替换一条代理条目；不给 `password` 保持已存的值，给空串清除 | JWT(admin) |
| PUT | `/api/v1/pull/proxies/order` | 重排：`{"ids":[...]}` 列出全部条目各一次 | JWT(admin) |
| DELETE | `/api/v1/pull/proxies/{id}` | 删除代理条目 | JWT(admin) |
| GET | `/api/v1/pull/credentials` | 凭据列表（每个 host 一条）：`{id, host, username, has_secret}`；镜像仓库是用户名加密码，模型站点只有令牌（`username` 为空，下载与取元数据时作为 `Authorization: Bearer` 发送）；密码或令牌从不返回 | JWT(admin) |
| POST | `/api/v1/pull/credentials` | 新建凭据（`host`、`username`、`secret`），加密存储；`host` 规范为不带协议与路径的 host（Docker Hub 的各种写法归为 `docker.io`），每个 host 一条 | JWT(admin) |
| PUT | `/api/v1/pull/credentials/{id}` | 替换凭据；`secret` 为空保持已存的值 | JWT(admin) |
| DELETE | `/api/v1/pull/credentials/{id}` | 删除凭据 | JWT(admin) |
| POST | `/api/v1/pull/credentials/{id}/test` | 从管理节点（经它的出站代理）试用凭据：有用户名的按镜像仓库登录 `/v2/`，只有令牌的按 Hugging Face 的 `GET /api/whoami-v2` 校验；200 `{ok, message}`，被拒 422 `CREDENTIAL_LOGIN_REJECTED`（`host`），连不上 502 `CREDENTIAL_LOGIN_FAILED`（`host`、`detail`） | JWT(admin) |
| GET | `/api/v1/pull/registry-credentials` | 已删除：本路由及其 `POST`、`PUT/DELETE {id}`、`POST {id}/test` 回 410 `SETTING_REMOVED`（`route` 为 `/api/v1/pull/credentials`） | JWT(admin) |
| GET | `/api/v1/server/listen` | server 的监听地址：已保存的 `console`/`agent`/`gateway` 地址、当前实际打开的 `running` 套接字、`restart_pending`（保存的地址要重启才生效）、`config_path`，以及 `editable`/`overridden`（未用 server.yaml 启动或被 `--addr` 覆盖时不可编辑） | JWT(admin) |

`PUT /api/v1/settings/image_registry_rewrites`、`image_registry_mirrors` 及镜像加速预设设置回 410 `SETTING_REMOVED`，镜像来源改由 `/pull/source-rules`（本节上表）表达。`PUT /api/v1/settings/network` 与 `network:<节点 id>` 同样回 410 `SETTING_REMOVED`（`route` 为 `/api/v1/pull/proxies`）：代理改为按节点标签的代理条目，URL 改写改为站点规则；升级时旧的全局与节点设置自动转换（全局改写为不限节点、用于管理节点的站点规则，节点覆盖为限定该节点、排在前面的规则与代理条目，节点生效的代理不变）。网络加速预设（`GET /api/v1/network/presets`）已删除。

## Metrics / Logs

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/metrics/query` | 时序指标查询（`metric`、`range`、`step`、`node`/`gpu`/`gpu_uuid`/`instance`/`benchmark`/`point`/`iface`/`hca`/`role` matcher）。返回 `{"items": [{"labels": {…}, "points": [[unix_ms, value], …]}], "total"}`，每个 label 组合一条序列。没有数据且指标名不在指标目录（`GET /api/v1/monitor/metrics`）里时回 400 `METRIC_UNKNOWN`，`params.similar` 给出最接近的已登记名；目录里有但窗口内没有数据时正常返回空 | JWT |
| GET | `/api/v1/logs/search` | 日志检索/tail（`q`、`instance` 前缀、`instances`（逗号分隔 uuid 精确匹配集合，最多 64 个，与 `instance` 互斥）、`node`、`level`、`source`、`from`/`to`、`cursor_ts`/`cursor_id`、`limit`、`with_total`；按到达序倒序分页；未指定时间边界表示该方向不限制，均未指定时查询所有尚未清理的日志；列表与总数使用同一范围，页面默认显式传最近 24h）。返回 `{"items": [{"id", "instance", "node", "level", "source", "time_ms", "line"}], "total", "total_capped", "truncated"}`（后三项按需出现） | JWT |
| GET | `/api/v1/logs/context` | 单条命中的上下文窗口（`instance`、`ts`、`id`、`before`/`after` 取值 0–500），等价 grep -C。返回 `{"items": [日志行], "match_index"}` | JWT |
| GET | `/api/v1/logs/histogram` | 时间桶 × 级别计数（同 search 过滤参数，含 `instances`，+ `bucket` 毫秒，1s–24h）。返回 `{"items": [{"start_ms", "counts": {"error": N, …}}], "total"}`；桶按 UTC 对齐 | JWT |
| GET | `/api/v1/monitor/overview` | 监控中枢集群层快照（`range` ∈ `15m\|1h\|6h\|24h\|168h`，默认 `1h`，或自选时间段 `from`+`to`（见下文）；信号行（只报事实：数值、明细与近期走势，不带严重程度）、服务表（每行带该部署的 `health` 健康检查，按健康状况排序：失败、超时、无可用副本、检查中、正常、未开启，同档按名称）、显存条、集群 tok/s、容量事件） | JWT |
| GET | `/api/v1/monitor/deployments/{id}` | 监控中枢服务层快照（`range` ∈ `15m\|1h\|6h\|24h\|168h`，默认 `1h`，或 `from`+`to`；请求对账、首 token 拆分、图表、副本表、`log_instances`（该部署存活 + 窗口内近期停止的副本 uuid，最多 64 个，供跳转 `/logs/search?instances=...`）；`sources` 给出 `ttft`/`e2e`/`prompt_tokens`/`generation_tokens`/`running` 各取自引擎（`engine`）还是网关（`gateway`），没有数据的信号不出现，见[推理指标词表](inference-metrics.md#网关指标)；`health` 同部署详情的健康检查；PD 分离部署另有 `pd`：`effectiveness_pct`、`fallback_rate_pct`、`fallbacks`（降级原因 → 次数）、`prefill_stage_avg_ms`、`prefill_stage_p95_ms`（网关等 prefill 实例的时间，与平均值同一窗口（近 5 分钟），P95 取请求分段直方图）、`prefill_waiting_avg`、`avg_prompt_tokens`，以及 `kv_transfer`：`exposed`（该 PD 模式的引擎是否导出 KV 传输指标，`vllm-ascend-mooncake` 为 `false`，其余字段都为 `null`）、`avg_ms`/`p95_ms`（每次传输）、`avg_mb`、`mb_per_s`、`failed`（decode 拉取 KV 失败）、`expired`（prefill 实例的 KV 租约到期未被拉走），窗口内次数，没有数据为 `null`；未知 id 返回 404） | JWT |
| GET | `/api/v1/monitor/deployments/{id}/requests` | 部署的逐请求视图。`view=live`（默认）：进行中的请求，按等首字最久 → 思考中 → 输出中排序，`counts` 给全部在途请求的 `waiting`/`thinking`/`answering` 数；`view=history`：已结束的请求倒序，`next_cursor` 翻页（`cursor`，游标分页），时间范围是 `range`（截至现在）或 `from`+`to`（Unix 毫秒，成对给出），响应带实际采用的 `from_ms`/`to_ms` 和 `range_total`（该范围内网关结束的全部请求数，不受筛选与日志抽样影响，按 5 分钟档统计）。`limit` 1..500（实时默认 50），`key` 只看一个 API key，`instance` 只看一个副本；历史另可按 `results`（逗号分隔：`completed` 正常完成、`length` 长度截断、`client_closed` 客户端退出、`interrupted` 连接中断、`error` 出错）与端到端时间 `min_ms`/`max_ms` 筛选，每条记录的 `result` 是它的结果，`segments` 是服务端把它的时间切成的各阶段（`kind` ∈ `gateway`/`pd_prefill`/`preprocess`/`queue`/`prefill`/`waiting`/`thinking`/`answering`/`tail`；`gateway` 是网关自身从收到请求到转给副本的耗时（扣掉 `pd_prefill`：PD 顺序交接时网关等 prefill 实例的时间），`preprocess` 是引擎上报了排队时，首字前的等待里引擎没有计入的部分，`ms`，加起来等于端到端时间或在途时长；拆分规则见[请求时间线与阶段拆分](request-timeline.md)）。PD 分离部署的请求带 `pd`（其余为 `null`）：`prefill_instance_uuid`（处理 prefill 的实例，降级时为空）、`prefill_ms`（网关等它的时间；并发交接与在途请求为 `null`）、`prefill_queue_ms`/`prefill_compute_ms`（prefill 实例自报的排队与 Prefill，没报为 `null`）、`fallback`（由 decode 单独完成的原因，没有为空）；还在 prefill 阶段的请求出现在在途视图里，`dispatch_ms` 为 `null`。每条请求带 `trace_id`（W3C trace id，32 位十六进制；调用方带了 `traceparent` 时是调用方的 trace，否则是网关新起的）与 `deployment_id`。时间字段是相对网关收到请求的毫秒，未发生或未观察到为 `null`；`weight` 是抽样权重。未知部署 404，非法参数或游标 400 | JWT |
| GET | `/api/v1/monitor/requests/lookup` | 按 trace id 或请求 ID 跨部署查找已结束的请求。`trace_id`（32 位十六进制，大小写均可）返回该 trace 的全部请求，`request_id` 返回该请求 ID 的请求，二者给且只给一个；`from`+`to`（Unix 毫秒，成对给出，间隔不超过 24 小时）限定接收时间，不给时：网关自己起的 trace id 带有时间，就查它前 10 分钟到后 1 分钟，查不到再查最近 1 小时；其余查最近 1 小时。`limit` 1..500（默认 100）。响应 `{as_of, from_ms, to_ms, window, requests, deployments}`：`window` 是实际查的窗口来源（`requested` / `trace_time` / `default`），`requests` 与请求视图同形、按接收时间倒序，`deployments` 是这些请求所属部署的 `{id, name}`（已删除的部署 `name` 为空）。请求日志超出每秒预算时被抽样掉的正常请求查不到。参数不合法 400 | JWT |
| GET | `/api/v1/monitor/deployments/{id}/latency` | 部署的端到端延迟拆解：`range` 内完整回复（正常完成或长度截断）的 `requests` 数、`end_to_end`（`mean_ms`/`p50_ms`/`p95_ms`，没有请求时为 `null`）、`segments`（按阶段顺序，每段 `kind`、`mean_ms`（对全部完整回复取平均，没有该段的按 0 计，各段之和等于端到端平均）、`requests`（有该段的请求数）、`p50_ms`/`p95_ms`（只算有该段的请求，四分之一倍频程分档））与 `speeds`（`prefill_tok_s`/`thinking_tok_s`/`answering_tok_s`/`decode_tok_s`，token 数之和 ÷ 时长之和，没有数据为 `null`）。按全部请求累加，不受日志抽样影响。未知部署 404，非法 `range` 400 | JWT |
| GET | `/api/v1/monitor/distributions` | `range` 内结束的请求分布：`input_output`（输入档 × 输出档）与 `uncached_ttft`（非缓存输入档 × 首 token 时间档）两个网格的 `{x, y, n}`，档位下界 `token_buckets` 与首 token 档上界 `ttft_bounds_ms`，`coverage` 给总请求数、带 usage 的数与报了缓存命中的数；`deployment` 可选，缺省为全集群，未知部署 404 | JWT |
| GET | `/api/v1/monitor/metrics` | 指标目录：平台存储的全部序列（`{"items": [{"series", "name", "source", "kind", "unit", "quantiles", "labels", "description"}], "total"}`）。`source` ∈ `engine\|gateway`（推理词表 `internal/inferencemetrics`）或 `gpu\|dgpu\|node\|network\|kv_disk\|benchmark\|gateway`（平台自身的序列，`internal/platformmetrics`，带 `labels` 与英文 `description`）；推理直方图展开为分位数 gauge（`quantiles: true`）与 `_sum`、`_count`。指标浏览器据此分组列出全部指标 | JWT |
| GET | `/api/v1/monitor/series` | 指标浏览器（`metric` 必填，`range` ∈ `15m\|1h\|6h\|24h\|168h`，默认 `1h`，或 `from`+`to`（`step` 默认约 240 个点、取 10 秒整数倍）；`by` ∈ `node\|deployment\|instance\|gpu`，`fn` ∈ `avg\|sum\|max\|rate`，默认 `avg`；`step` 默认按 `range` 推导，且不会低于该推导值；结果超过序列/点数上限时返回 400；未知指标返回 `[]`） | JWT |

监控图表的自选时间段：`overview`、`deployments/{id}`、`deployments/{id}/latency`、`distributions`、`series` 都接受 `from`+`to`（Unix 毫秒，成对给出，与 `range` 互斥），跨度 1 分钟到 30 天、`to` 不能在未来，否则 400 `MONITOR_RANGE_INVALID`（`detail`）。此时图表、分布、延迟拆解与信号按截至 `to` 的这段时间计算，不缓存；服务列表、副本、健康检查等当前状态仍是现在的。

## Analytics（用量分析）

长期统计与数据保留，见 [ADR 0055](../adr/0055-long-term-statistics.md)。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/data-retention` | 数据保留设置：`{value, limits, server_time_zone}`。`value` 的字段：`metrics_hours`、`logs_hours`（指标原始样本、容器日志）、`request_log_days`、`request_log_max_mb`（逐请求记录 ux.db，天数与容量哪个先到按哪个清理，当天不清）、`usage_hourly_days`、`usage_daily_days`（Token 用量，主库）、`stats_hourly_days`、`stats_daily_days`（服务质量、GPU 利用、卡时，stats.db）、开关 `quality`、`gpu`、`gpu_time`、统计时区 `time_zone`（IANA 名）。`limits` 是每个数值字段的 `{min, max}` | JWT |
| PUT | `/api/v1/data-retention` | 整体替换设置并立即生效（ux.db 的上限立即改，清理与按天汇总立即跑一轮）。body 必须给全 `value` 的每个字段、不能有多余字段；超出 `limits`、按天保留短于按小时、时区不存在时 400 `FIELD_INVALID`（`field`、`detail`）且不写入。改短的部分在后台分批删除。返回同 GET。`PUT /settings/data_retention` 回 400 `SETTING_HAS_OWN_ROUTE`，`GET /settings` 不含它 | admin |
| GET | `/api/v1/data-retention/status` | 各类数据现在的情况：`stores[]`（`kind` ∈ `metrics`/`logs`/`requests`/`usage`/`quality`/`gpu`/`gpu_time`，`file` 所在文件（`main` 是主库），`file_bytes` 文件大小（主库为 `null`；`quality`/`gpu`/`gpu_time` 同在 stats.db，报同一个数），`hourly_rows`/`daily_rows`（长期统计的行数，原始数据为 `null`），`earliest_ms` 最早还能查到的时间）、`live_since_ms`（开始实时累加的时间，之前的数据来自升级时的一次性补算）、`backfill`（`usage_done`/`usage_through_ms`/`quality_done`/`quality_through_ms`：补算进行到哪）、`retain`/`rollup`（最近一次清理、按天汇总的 `at_ms` 与 `error`）、`usage_flush`（最近一次 Token 用量写入的 `at_ms`、`rows`、`ms`、`error`） | JWT |

统计查询（JWT）。公共参数：`from`、`to`（Unix 毫秒，必填）、`granularity`（`hour` / `day`，不填时跨度不超过 2 天按小时、否则按天；按小时最多 7 天，按天最多 1825 天）。按天时范围扩展到统计时区的整天；已汇总的天读按天数据，没汇总的天（今天，或 server 停机时跨过的天）由它的小时数据现算。响应都带 `range`：`from_ms`、`to_ms`、`granularity`、`time_zone`、`earliest_ms`（这一粒度最早还有数据的桶）、`live_since_ms`（之前的桶来自升级时的一次性补算：抽样行按权重计，当时网关不识别 usage 的 `/v1/messages`、`/v1/responses` 请求没有 token）、`buckets`（各桶起点，`series` 与它一一对应）。参数不合法 400 `ANALYTICS_RANGE_INVALID` / `ANALYTICS_RANGE_TOO_LONG` / `ANALYTICS_VALUE_INVALID`（`field`、`detail`）。

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/analytics/usage` | Token 用量。筛选：`deployment`（部署 id）、`key`（API key id，`none` 为没有 key 的请求）、`source`（`api` / `playground`）。`totals` 与 `series[]` 是计数：`requests`、`no_usage`（没拿到 usage 的请求）、`prompt_tokens`（总输入）、`completion_tokens`、`reasoning_tokens`（`reasoning_reported` 个请求报了）、`cached_tokens`（`cache_reported` 个请求报了缓存命中，命中率 = `cached_tokens` ÷ `cache_reported_prompt`）；`rows[]` 每个「key × 用户 × 部署 × 来源」一行，带 `api_key_name`/`api_key_prefix`/`api_key_revoked`/`api_key_deleted`（吊销改为保留前就删掉的 key）、`user_name`、`deployment_name`/`served_name`/`deployment_deleted`、`source`、`first_ms`/`last_ms`（用到的第一个、最后一个桶） | JWT |
| GET | `/api/v1/analytics/quality` | 服务质量，`deployment` 可选。`totals`、`series[]`、`rows[]`（按部署）：各结果数（`completed`、`length`、`client_closed`、`interrupted`、`error`）、`ttft_p50_ms`/`ttft_p95_ms`（流式完整回复的首 token）、`e2e_p50_ms`/`e2e_p95_ms`（完整回复的端到端）、`output_tokens_per_sec`；分位数由四分之一倍频程分档还原，误差约 10%，没有数据为 `null` | JWT |
| GET | `/api/v1/analytics/gpus` | GPU 利用。`group_by` ∈ `model`（默认）/ `node` / `gpu`（表格与热力图的行），`metric` ∈ `util`（默认）/ `power` / `temp` / `mem`（热力图的值，桶内均值），筛选 `label=键=值`（可重复，同节点列表：按节点当前的书写标签与系统标签匹配，已删除的节点不再匹配）、`model`（卡型）。`card_count` 与 `by_model[]{model, cards}` 是筛选覆盖的当前卡数。覆盖不超过 256 张卡时 `detail: true`，`cards[]` 每张卡一行：`node_name`、`gpu_index`、`gpu_uuid`、`gpu_model`、`present`（现在仍在在线节点上）、`deployments`（现在占用它的部署）、`util_avg`/`util_max`、`mem_used_avg_bytes`/`mem_used_max_bytes`/`mem_total_bytes`、`power_avg_watts`、`temp_avg_celsius`/`temp_max_celsius`；超过时 `detail: false`、`cards` 为空。`groups[]`（按型号或节点时）每组一行：`key`、`label`、`cards`、`nodes`、`models[]`、`deployments[]{name, cards}`、`unassigned`（无部署占用的卡数）与同上的统计值，取自按节点、按型号的统计。`heat[]`：`{key, label, values[]}`；按节点且超过 100 个节点时只含该指标最高与最低的各 50 个，`heat_total` 是全部行数。按卡分组而覆盖超过 256 张时 400 `ANALYTICS_TOO_MANY_CARDS` | JWT |
| GET | `/api/v1/analytics/gpu-time` | 卡时，`deployment` 可选。`rows[]` 每个「部署 × 卡型」一行：`gpu_hours`（dGPU 按显存份额折算）、`whole_gpu_hours`、`avg_instances`（到现在为止的平均副本数）、`max_instances`、该部署同一时段的 `prompt_tokens`/`completion_tokens`、`gpu_hours_per_million_output`/`gpu_hours_per_million_tokens`（没有 token 为 `null`）；`series[]` 每个部署各桶的卡时；`capacity[]` 每种卡型的 `total_gpu_hours`（在线节点上平台可用的卡）、`schedulable_gpu_hours`、`allocated_gpu_hours` | JWT |
| GET | `/api/v1/analytics/key-usage` | 每个有用量的 API key 今天与近 7 天（含今天）的计数：`{items:[{prefix, today, last_7_days}], time_zone}`，供 API 令牌页 | JWT |

## AI Helper（AI 助手）

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/ai/configs` | AI 助手 LLM 接入配置列表 | JWT |
| POST | `/api/v1/ai/configs` | 新增 LLM 接入配置 | JWT |
| DELETE | `/api/v1/ai/configs/{id}` | 删除 LLM 接入配置 | JWT |
| GET | `/api/v1/ai/available` | AI 助手是否可用（是否已配置 LLM） | JWT |
| POST | `/api/v1/ai/ask` | 让 AI 助手执行一个任务。`task` 取值：`instance.diagnose`（带 `instance_uuid`）、`model.fit`（带 `model_id`）、`deployment.unscheduled`（带 `deployment_id`）、`node.health`（带 `node_uuid`）、`ask`（必须带 `question`，可选附带上述某一个对象）。对象按引用指定，server 自己组装上下文；可选 `question`；追问时带 `history:[{role:user\|assistant,content}]`（最多保留最近 10 轮）。server 组装好上下文后跑带只读工具的 agent 循环（工具以提问用户的身份在进程内读本 API 的 GET 接口，见 [使用 AI 助手](../how-to/use-ai-helper.md)），返回 `{answer, steps:[{tool,args,ms,error?}], calls, usage:{prompt_tokens,completion_tokens}}`。请求头带 `Accept: text/event-stream` 时改为 SSE 流式返回：`step`（`{id,tool,args,status:running\|done\|error,ms,error?}`，每次工具调用开始与结束各一条）、`text` / `reasoning`（`{delta}`）、最后一条 `done`（`{calls,usage,first_text_ms,total_ms}`）或 `error`（`{error}`），空闲时每 10s 一条 `: ping` 注释；问模型之前就失败的情况（未配置、对象不存在）仍以状态码返回。未配置 LLM 回 503 `AI_ENDPOINT_NOT_CONFIGURED`，`task` 非法回 400 `AI_TASK_INVALID`，缺对象或同时给了多个对象回 400 `AI_TARGET_REQUIRED`，同一用户已有 2 个提问在处理时回 429 `AI_ASK_BUSY`（`params.limit`），实例/模型/部署/节点不存在回 404，LLM 端点失败回 502 | JWT |
| GET | `/api/v1/ai/instances/{uuid}/findings` | 失败实例日志命中的已知故障：`{items:[{code, evidence:[{line}]}], total}`，`code` 稳定、文案由前端渲染。不需要 LLM；实例不存在 404 | JWT |
| GET | `/api/v1/ai/models/{id}/fit` | 模型在在线集群每种加速卡上的物理适配（讲硬件事实，不讲当前空闲）：`{model_id, weight_bytes, dtype, gpu_memory_utilization, rows:[{vendor,name,memory_bytes,nodes,cards,unusable,max_cards_per_node,dtype_supported,dtype_note?,fast_link_cards,fits,tp,per_gpu_bytes,usable_bytes,headroom_bytes}]}`。`cards` 是可用卡数，`unusable` 是故障卡数（与前端健康等级同一规则）；`dtype_supported` 表示该卡能否计算模型的精度；`fast_link_cards` 是单节点内 NVLink 等高速互联组的最大卡数（1 表示只有 PCIe，0 表示未知；每种卡型最多抽样 8 个节点）；`tp` 是单节点内放得下权重与引擎预留的最小 2 的幂，放不下时 `fits:false`、`tp:0`。数据按「节点 × 卡型」分组读取。不需要 LLM；模型不存在回 404 | JWT |

| POST | `/api/v1/mcp` | MCP 服务（Streamable HTTP，只返回 JSON，不开 SSE；`GET` 回 405）。JSON-RPC 2.0，单条消息，支持 `initialize`、`ping`、`tools/list`、`tools/call` 与各类通知（回 202）。工具即 AI 助手的只读工具集，以调用者身份读本 API，同样脱敏、整形；工具失败以 `isError: true` 的结果返回，同一用户同时最多 4 个工具调用。服务关闭时回 403 `MCP_DISABLED`。接入方式见 [通过 MCP 接入](../how-to/use-mcp.md) | JWT 或平台令牌（`read` 即可） |
| GET | `/api/v1/mcp/status` | MCP 服务状态：`{enabled, path, protocol_version, tools}` | JWT |
| PUT | `/api/v1/mcp/settings` | 开关 MCP 服务：`{enabled}`，返回新的状态；默认关闭 | JWT(admin) |

未配置 LLM 时确定性估算器产出仍完整可用；`ai/ask` 仅在配置了 LLM 后可用；MCP 不需要 LLM。

## Playground

| Method | Path | 说明 | 鉴权 |
|---|---|---|---|
| GET | `/api/v1/playground/models` | 可在 Playground 调用的已部署模型列表，不包含全局设置中的 AI 助手外部模型；`html_preview.allow_network` 是 HTML 预览的网络策略（来自安全设置） | JWT |
| GET | `/api/v1/playground/openapi.json` | Playground 代理的 OpenAPI 描述 | JWT |
| ANY | `/api/v1/playground/v1/*` | Playground 内部代理（管理 JWT 保护，非 `/v1` 网关 key） | JWT |

Playground 与 `/v1` 网关的边界、外部模型命名 `external:<config-id>:<model>` 见
[`openai-gateway.md`](openai-gateway.md)。

## 未覆盖内容

- 数据面 `/v1/*`（chat/embeddings/rerank/audio/image/video/realtime 等 OpenAI 兼容 endpoint）见
  [`openai-gateway.md`](openai-gateway.md)，不在本文重复。
- agent 侧 yamux 子协议（`ctrl`/`metrics`/`logtail`/`tunnel`/`model.read`/`model.write`/
  `image.read`/`image.write`/`terminal`）不是 HTTP 路由，属于 `internal/proto` 控制面协议，不在本文范围。
