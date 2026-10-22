# 用户体验改进计划

加入计划的安装每天向发布方发送一份匿名报告，说明它运行在什么环境里、用来做什么、当天执行了哪些任务、在哪里出了错。发布方据此决定先支持哪些硬件与引擎、先修哪些错误。默认加入；本页列出报告的**全部字段**，报告里没有本页以外的内容（`internal/server/usagereport` 的测试保证二者一致）。

## 开关

- 设置 → 通用 →「版本与更新」里的「用户体验改进计划」开关，写入 `server.yaml` 的 `usage_report` 并立即生效（[配置](configuration.md#server-配置文件-serveryaml)）。
- **私有化交付**在 server 首次启动前手动写入 `usage_report: false`。
- 退出后：不发送日报；自动检查更新时不附带装机规模；退出期间的进程内计数随即丢弃。重新加入时立刻发送一份。检查更新与[发布方通知](#发布方通知)不受影响。
- 开关旁的叹号悬停显示简要说明。下一份报告的原文可以用 `GET /api/v1/usage-report/preview` 取得，与实际发送的字节完全相同。

## 发送方式

- **只有 server 发送**，agent 不改动、不外连。每天一次 HTTPS POST，报告未压缩不超过 128 KiB（超出时每个分布再裁剪并标记 `truncated`）。
- **时间**：首次启动、以及关闭后重新打开时，立刻发送一份（覆盖前一个 UTC 日）；之后每天一份，覆盖上一个 UTC 日，在次日 00:30 UTC 之后、按安装标识散列错开的 0–6 小时内发送。server 停机错过的日子不补发，只发最近一天。
- **范围**：只有带 `server.yaml` 启动的正式安装才发送；没有 `server.yaml` 的 server（测试、开发时直接运行）从不发送。环境变量 `GPUWORKS_USAGE_REPORT=off` 也能强制关闭（CI 与 l2 测试都设置了它）。
- **去向**：两个收集端，阿里云日志服务（SLS）上海的 WebTracking 日志库（`gpuworks-usage.cn-shanghai.log.aliyuncs.com`，deflate 压缩）与 Cloudflare 上的收集端（`usage.gpuworks.ai`，gzip 压缩）。地址编译在二进制里，不可配置。
- **选路**：发送前同时向两个收集端各发一个不带内容的 GET 测速（它们只回 4xx，不写数据，5 秒超时），报告发给先应答的那个；SLS 在前，Cloudflare 只有快出 50 ms 以上才排到前面。发送失败再试另一个。测得的毫秒数和最终选择写进报告的 `delivery`。正常情况下每天只有一份报告送达一个收集端。
- **失败**：不重试、不告警、不影响任何业务；当天在进程内的计数并入下一天的报告。离线环境发不出去，没有副作用。设置页显示最近一次投递的结果。
- **新增的持久化**：只有一个设置项 `usage_report_last_day`（最近送达的报告所覆盖的日子）。

## 收集端记录的来源地址

报告正文不含任何地址，但收集端会记录**报告请求的来源公网 IP**，即 server 访问外网时的出口地址（通常是机房的 NAT 出口，不是节点或 server 的内网地址），用于按国家与省份统计分布、识别伪造的报告。SLS 把它与报告一起保存 180 天；Cloudflare 收集端把它和 Cloudflare 判定的国家、地区记在报告对象的元数据里。

## 从不发送

主机名、内网 IP 与 MAC 地址、节点或部署的名称、用户名、标签、本地路径、私有镜像仓库地址、云账号与区域、错误信息原文与错误参数、日志、提示词与回复、你自己的模型名称（非公开仓库与本地模型一律记为 `custom`）。版本号只保留数字部分（内核 `6.8.0-138-generic` 记为 `6.8`），避免带出主机的定制构建名。

## 字段

分布（下表类型为「分布」）是 `{"名称": 数量}` 对象，按数量取前 16 项，其余合并为 `other`，名称最长 64 字节、为空记为 `unknown`；`usage.routes` 取前 64 项，`errors.events` 与 `errors.api_errors` 取前 32 项。

| 字段 | 类型 | 含义 |
|---|---|---|
| `schema` | 整数 | 报告格式版本，当前为 1 |
| `install_id` | 字符串 | 安装标识：由 `master.key` 单向派生（HMAC），推不出密钥，只用于去重；与检查更新带的 `id` 相同 |
| `day` | 字符串 | 报告覆盖的 UTC 日（`YYYY-MM-DD`） |
| `truncated` | 布尔 | 报告超过上限被再次裁剪时为 `true`，否则不出现 |
| `delivery.endpoint` | 字符串 | 测速后选中的收集端：`sls` 或 `collector` |
| `delivery.probe_ms` | 对象 | 每个收集端应答测速请求的毫秒数，`-1` 表示没有应答 |
| `server.version` | 字符串 | server 版本 |
| `server.channel` | 字符串 | 跟随的发布渠道（`nightly` / `release`） |
| `server.os` / `server.arch` | 字符串 | server 的操作系统与 CPU 架构 |
| `server.schema_version` | 整数 | 数据库迁移版本 |
| `environment.nodes` / `environment.online_nodes` | 整数 | 节点总数 / 在线节点数（报告生成时） |
| `environment.agent_versions` | 分布 | 每个 agent 版本的节点数 |
| `environment.os` | 分布 | 节点操作系统（`linux`、`darwin`） |
| `environment.kernels` | 分布 | 节点内核版本（主.次） |
| `environment.arch` | 分布 | 节点 CPU 架构 |
| `environment.vendors` | 分布 | 节点的加速卡厂商（`nvidia`、`ascend`、`ppu`、`amd`、`hygon`、`apple`） |
| `environment.frameworks` | 分布 | 计算框架与主.次版本，如 `cuda 12.4`、`cann 8.0` |
| `environment.drivers` | 分布 | 厂商与驱动主版本，如 `nvidia 550` |
| `environment.runtimes` | 分布 | 容器运行时（`docker`、`nerdctl`、`udocker`、`host`） |
| `environment.connect_modes` | 分布 | 连接方式（`agent_dial`、`server_dial`） |
| `environment.onboarding` | 分布 | 接入方式：`ssh`（经 SSH 安装）、`cloud`（云厂商实例）、`agent`（脚本或手动安装） |
| `environment.gpu_models` | 分布 | 节点自己拥有的卡，按型号计数 |
| `environment.card_ownership` | 分布 | 平台不能使用的卡及原因（`passthrough`、`no_driver`、`held_elsewhere`） |
| `environment.card_health` | 分布 | 有健康问题的卡，按健康码计数 |
| `environment.providers` | 分布 | 已配置的云厂商种类（不含账号、区域） |
| `environment.storages` | 分布 | 存储类型（`local`、`nfs`、`nfs_cache`、`shared_dir`） |
| `usage.deployments` | 整数 | 部署总数 |
| `usage.deployment_engines` | 分布 | 每个推理引擎的部署数 |
| `usage.deployment_states` | 分布 | 部署期望状态（`active`、`stopped`、`deleting`） |
| `usage.multi_node` / `usage.pd` / `usage.sleepable` | 整数 | 多机部署、PD 分离部署、可休眠部署的数量 |
| `usage.dgpu` | 分布 | 使用 dGPU 的部署，按后端计数 |
| `usage.template_sources` | 分布 | 部署所用模板的来源：`upstream`（内置）、`user`、`none` |
| `usage.templates` | 分布 | 使用中的内置模板（模板的模型 ID） |
| `usage.models` | 分布 | 部署的模型：公开仓库记为 `huggingface:<仓库>` / `modelscope:<仓库>`，其余记为 `custom`。「公开」只在能确定时成立：部署用的是内置模板，或平台没有为该模型站点配置访问凭据（所有下载都是匿名完成的） |
| `usage.instance_states` | 分布 | 实例状态 |
| `usage.gateway_requests` | 分布 | 当天经网关的请求数，按部署的引擎计数 |
| `usage.logins` | 分布 | 当天管理登录次数：`ok` / `failed` |
| `usage.routes` | 分布 | 当天管理 API 的调用次数，按「方法 路由模板」计数（如 `POST /nodes/{uuid}/upgrade`），不含路径参数的值 |
| `tasks.deployments_created` | 分布 | 当天新建的部署，按引擎计数 |
| `tasks.instances_started` | 整数 | 当天创建的实例数 |
| `tasks.instances_ready` / `tasks.instances_failed` | 整数 | 其中到报告生成时已就绪 / 已失败的数量 |
| `tasks.ready_seconds` | 分布 | 就绪耗时分段：`<30s`、`30s-2m`、`2m-10m`、`10m-30m`、`30m-2h`、`>2h` |
| `tasks.model_files` | 分布 | 当天创建的模型文件，按「获取方式 状态」计数（如 `download present`、`peer failed`） |
| `tasks.model_bytes` | 分布 | 当天下载的模型文件大小分段：`<1G`、`1G-10G`、`10G-100G`、`100G-500G`、`>500G` |
| `tasks.benchmarks` | 分布 | 当天创建的评测，按「搜索模式 数据集种类 状态」计数 |
| `errors.events` | 分布 | 当天部署时间线事件，按事件码计数（如 `UNIT_RETRY_EXHAUSTED`、`DEPLOYMENT_UNSCHEDULABLE`） |
| `errors.api_errors` | 分布 | 当天管理 API 返回的产品错误，按错误码计数 |
| `errors.panics` | 分布 | 当天被恢复的 panic，按发生处最内层的平台函数名计数（不含文件、行号与参数） |
| `errors.images_failed` / `errors.model_files_failed` | 整数 | 报告生成时处于失败状态的镜像副本 / 模型文件数量 |
| `errors.protocol` | 分布 | 当天 agent 连接问题，按原因计数（`proto_mismatch`、`bad_token`、`stale_session`、`data_lane_unavailable`） |

## 发布方通知

页头的铃铛列出发布方发给本安装的通知：安全公告、重要版本、社区活动。

- 通知是发布通道里的签名文件 `notices.json`，与版本索引用同一把密钥签名（[ADR 0040](../adr/0040-signed-multi-source-distribution.md)），随检查更新一起拉取（同一渠道每小时最多一次；`auto_check_updates` 开启时另每 6 小时后台拉取一次）；没有通过验签的文件一律拒绝。
- 是否适用由 server 用本地事实判断（渠道、server 构建时间、集群里的加速卡厂商），**判断不向发布方发送任何信息**，也与是否加入改进计划无关。
- 通知只有纯文本与一个 https 链接，不能触发任何动作。适用于本安装的安全通知另外在 server 日志里写一条 warn，便于没人登录控制台的环境用日志告警发现。
- 已读按用户记录；发布方撤回的通知，其已读记录随下一次拉取删除。

设计取舍见 [ADR 0054](../adr/0054-usage-report-and-notices.md)。
