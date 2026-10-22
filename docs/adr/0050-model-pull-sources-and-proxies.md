# ADR 0050: 来源规则同时覆盖模型站点，代理按标签配置，凭据按 host 统一

日期：2026-10-17 · 状态：已接受（取代 [ADR 0048](0048-pull-settings.md)「后果」最后一条「模型下载的来源规则……是后续一步」；ADR 0048 其余决策保留）

依据：[模型拉取来源与拉取代理计划](../../artifacts/work/2026-10-17-model-pull-sources/plan.md)。

## 背景

镜像来源已由拉取设置统一（ADR 0048），模型下载仍靠网络设置里的 URL 改写，代理是全局加逐节点覆盖：

- 模型只有一个下载地址，镜像站挂了不回退原站；同一地址最多重试 16 次，一个坏镜像站能拖一个多小时；不能表达「这些节点只准走内网」。
- server 取模型元数据走启动参数 `--model-endpoint`，不读网络设置里的改写；浏览器写死 hf-mirror.com。节点能下、server 登记或建部署却连 huggingface.co 失败，部署准入每次都读且失败即拒绝。
- 没有 Hugging Face token，gated 与私有模型下载不了。
- 节点覆盖整体替换全局设置，只能逐台写，不能按标签；镜像与模型是两处设置、两套语义。

## 决策

1. **站点规则与镜像规则在同一张表里，种类由 `match` 决定。** `match` 以 `http://` 或 `https://` 开头是站点规则（站点根，不带路径；替代地址是可带路径的 http(s) 地址），否则是镜像规则。语法与匹配是叶子包 `internal/server/sourceurl` 的唯一实现。规则叠加：所有匹配规则的替代地址按规则顺序排入候选、去重，站点本身在最后；任一匹配规则关闭回源则丢弃站点本身，什么都不剩即 `WEIGHTS_FETCH_FORBIDDEN`。目录 URL 来源的模型只从自己的目录下载，规则不作用于它；ModelScope 与 Hugging Face 互不回退。
2. **候选一处计算。** `planning.PullSettingsFor` 取该节点适用的设置，`planning` 按节点算出模型文件任务的候选（规则替代地址在前、站点在后）；hub 的期望状态重放与 controller 的主动推送用同一入口，规则变更触发重新评估。server 自己读元数据的候选来自 `planning.ServerFetchCandidates`。
3. **agent 逐个候选下载（`internal/agent/modelfetch`）。** 连接、DNS、TLS 错误与任何 HTTP 错误状态（含镜像站缺仓库的 404、401/403）使该地址冷却 30 分钟（排末尾不剔除，与镜像拉取共用）并换下一个；单连接 5 分钟无数据为一次卡住，同一地址连续 2 次卡住换下一个；瞬时错误在有后续候选的地址上最多重试 6 次，最后一个地址保留 16 次且卡住只重试。版本固定为 commit 时，换地址接着续传部分文件，否则从头开始。进度写「正在从 <host> 下载」，切换后带旧 host 与原因。
4. **管理节点用显式勾选，不靠标签。** server 没有节点标签，所以规则与代理各有一个 `control_plane` 列（界面「用于管理节点获取模型元数据」「用于管理节点出站访问」，前者只用于站点规则）。管理节点读元数据逐个候选询问（每个 5 秒），从第一个应答的候选读完全部内容。读不到不阻断登记：响应带 `metadata_notice`（`MODEL_METADATA_UNREACHABLE` 列出试过的 host，或 `MODEL_METADATA_FORBIDDEN`），控制台指向勾选；部署准入每个模型只读一次，不因管理节点连不上仓库而拒绝。`--model-endpoint` 退化为进程内测试夹具参数：每个站点一条规则，指向该端点、不回源、勾选。
5. **代理是拉取设置里的有序条目，首个匹配生效，取代全局加节点覆盖。** 条目含 `http_proxy`、`https_proxy`、`no_proxy`、用户名与加密保存的密码、节点标签选择器、备注、`control_plane` 勾选。一台节点用第一条满足条件的条目，条目不叠加（代理是整体选择，叠加没有意义）；无匹配回到 agent 自己的环境变量。管理节点出站（元数据、logo、目录 URL 登记、镜像归档 URL 预览与探测、凭据测试登录）用第一条勾选的条目。节点详情的「拉取代理」卡片只读，docker 节点的 dockerd 写入沿用 ADR 0048 决策 8。
6. **`no_proxy` 取 Go 的 `NO_PROXY` 语义，只有一个实现。** `pkg/httpproxy` 自行实现（不引入 `golang.org/x/net`），语义与 Go 标准库一致：`*`、IP/CIDR、域名匹配自身及子域名、前导 `.` 或 `*.` 只匹配子域名、可带端口，loopback 与 localhost 永不走代理；agent 追加 loopback 与 server 地址。同一份列表用于 agent 自己的请求、nerdctl / udocker 命令行拉取（此前丢掉 `NO_PROXY`）和 dockerd drop-in。目录 URL 模型因此也经节点代理，内网文件服务器要写进 `no_proxy`。
7. **凭据一张表，按 host。** 新表 `source_credentials`（迁移复制原 `registry_credentials` 后删除旧表）即「凭据」，每个 host 一条：镜像仓库（用户名加密码，写临时 `DOCKER_CONFIG`）与模型站点（只有 token，用户名为空）。站点 token 作为 `Authorization: Bearer` 只在 https 且 host 与凭据完全一致时发送，镜像站和其他域名永远拿不到；重定向沿用 Go 的行为，转发给同域名及其子域名（如站点的 CDN），不转发给其他域名。测试登录由管理节点经其代理发起：仓库 `/v2/`，token 请求 Hugging Face `GET /api/whoami-v2`。路由 `/api/v1/pull/credentials`，旧 `/api/v1/pull/registry-credentials*` 返回 410。
8. **浏览器取元数据读 server 的候选。** 控制台登记或刷新 Hugging Face 模型时，浏览器读 `GET /api/v1/pull/metadata-sources?source=huggingface`（管理节点的候选），不再写死 hf-mirror.com；只有浏览器确实读到内容才标记 `browser_metadata`，否则由 server 读。
9. **离线导入靠登记路径补全，不做上传。** 把模型目录拷到节点或共享存储，登记为本地路径，或登记 HF / ModelScope 模型后在节点上「登记路径」。`register-path` 用本地目录补齐模型缺失的元数据（config、README、大小，已有的不覆盖）；对 `huggingface-cli download --local-dir` 下载的目录，若 `.cache/huggingface/download/*.metadata` 全部指向同一个 commit，且这是该模型唯一的一份副本，则把模型固定到该 commit。完全离线的集群加两条「禁止」站点规则（无替代地址、关闭回源、勾选），让缺少来源的部署立即报错，不等公网超时。
10. **迁移与结转。** 迁移把全局 URL 改写转成站点规则（所有节点、允许回源、勾选管理节点），把每个节点覆盖里的改写转成限定 `gpuworks.ai/node=<uuid>` 的站点规则，排在最前；规则叠加，所以原先「覆盖替换全局」的节点现在依次试自己的地址、全局地址、站点本身。**代理不在迁移里转，而在 server 启动、密钥可用后结转**：迁移先于 secretbox 运行，拿不到加密密钥，没法加密代理密码。结转时每个节点覆盖变成限定该节点、合并了全局值的条目并排在最前（该节点用的代理不变），全局代理变成对全部节点生效且勾选管理节点的条目，URL 中内嵌的登录拆成用户名与加密密码；完成后删除旧的网络设置。每次下载手选的端点、代理列删除。
11. **删除。** 设置页「网络」卡片（代理、URL 改写、DNS 建议）、节点详情「网络覆盖」、`/settings/network`、`network:<id>`、`/nodes/{uuid}/network`（返回 410）、全部网络预设（hf-mirror、xget、DNS）。不做任何预设，示例写在 [how-to](../how-to/configure-pull-settings.md#常见网络配置示例)。

## 取舍

- **站点规则放进同一张表**而不是新表：用户心智是「某个来源去哪里拿」，镜像与模型同构；代价是 `match` 的种类要在校验和读取处分派（`store` 的种类分派校验），由 `sourceurl` 与 `pullref` 各管一种语法。
- **404 也换地址并冷却**：镜像站缺仓库与镜像站挂了对用户同样是「这里下不到」，冷却 30 分钟代价小于每个文件都重试一遍。代价是真正的 404（仓库名写错）会遍历所有候选后才失败。
- **固定 commit 才能跨地址续传**：分支名在不同镜像站可能指向不同提交，续传会拼出混合内容，所以非固定版本换地址时从头开始。
- **管理节点不自动推断**：server 没有标签，让管理员显式勾选比猜测更可预期；代价是第一次用要多点一下，连不上时提示指向勾选。
- **代理首个匹配而非叠加**，规则则叠加：代理是「走哪条路」，规则是「去哪些地址」。
- **新增持久化事实**：站点规则复用规则表加 `control_plane` 列，代理与凭据种类是管理员配置，无法推导，均用带类型的列；冷却状态不落库。

## 后果

- 新增 `/api/v1/pull/proxies`、`/api/v1/nodes/{uuid}/pull-proxy`、`/api/v1/pull/credentials`、`GET /api/v1/pull/metadata-sources`、`POST /api/v1/pull/source-rules/test`（按节点探测草稿规则的每个地址）；`/api/v1/pull/registry-credentials*`、`/api/v1/nodes/{uuid}/network` 返回 410。
- 新增 `internal/server/sourceurl`（叶子）与 `internal/agent/modelfetch`；`pkg/httpproxy` 支持 `no_proxy`。
- 部署页的权重预估写出候选顺序，被禁止时写「无法下载」；调度原因 `weights_fetch_forbidden`，下载接口 409 `WEIGHTS_FETCH_FORBIDDEN`，规则变更重新评估部署。

## 已知限制

- bench 的 tokenizer 仍由 agent 直接用 `http.DefaultClient` 从 huggingface.co 取（`pkg/bench/tokenizer/ref.go`），不走代理和规则（见 `todo.md`）。
- ModelScope 模型 logo 是任意 CDN 地址，不在规则之内，不可达时静默没有 logo（见 `todo.md`）。
- ModelScope 私有仓库的凭据尚未确认，目前只确认了 Hugging Face。
