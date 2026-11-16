# 插件协议与线上格式（gwx/1）

插件包清单、发布者证书、信任更新、吊销列表、签名信封、`gwx/1` 的握手、健康与错误消息，以及插件推送的表，线上格式都定义在独立的 Go 模块 `pkg/extapi`（模块路径 `gpuworks.ai/gpuworks/pkg/extapi`）里。它只依赖标准库，插件只依赖它，不依赖 server。

这些格式已经冻结，本页逐个说明。server 端的插件宿主（安装与校验插件包、拉起插件进程、执行推送表）**尚未实现**：本页写到的合并、宽限、时间线与吊销语义是这些格式的约定，由将来的插件宿主执行，设计与取舍见 [ADR 0071](../adr/0071-installable-extensions.md)；编译进 server 的模块契约见[扩展点](../dev/extensions.md)。

格式以黄金文件语料 `pkg/extapi/testdata/golden/` 为准。本页的每个 JSON 示例都与语料里的一个文件逐字节相同，页面源文件里示例上方的注释写明是哪个文件，`pkg/extapi` 的测试（`TestReferencePageExamplesAreGoldenFiles`）逐个比对，所以示例都是规范 JSON（单行、键已排序）。

## 通用规则

- **严格解码。** 读者只按自己认识的版本解码一个对象，遇到该版本没有定义的字段即拒绝（`PLUGIN_WIRE_UNKNOWN_FIELD`）。键区分大小写；重复的键、JSON 值之后的多余数据、类型不对的值、非法 UTF-8 都拒绝（`PLUGIN_WIRE_MALFORMED`）。
- **`display` 是唯一宽松的部分。** 清单、证书、吊销条目、可用性行与横幅可以带一个 `display` 对象：任意 JSON 对象，编码后至多 8 KiB，原样保留以便转交。它只用于显示，从不改变核心执行的结果，旧的读者可以丢弃它。
- **数字、时间与字节。** 数字都是整数，绝对值不超过 2^53−1；时间是 RFC 3339 的 UTC 整秒（`2026-01-02T03:04:05Z`），同一时刻的其他写法都拒绝；字节串是带填充的标准 base64。
- **集合排序。** 作为集合的列表（角色、权限、码、文件、根密钥 id、命名空间等）必须按字节排序且不重复，所以一个集合只有一种编码。
- **码与参数。** `code` 是 [ADR 0067](../adr/0067-runtime-outcomes-as-codes.md) 的 UPPER_SNAKE 码；`params` 至多 16 个，键是 lower_snake，值是至多 512 字节的字符串、整数或布尔。

### 版本与兼容

| 对象 | 版本从哪里来 | 当前版本 | 读到更高版本时 |
|---|---|---|---|
| 包清单 | `schema` | 1 | `PLUGIN_MANIFEST_TOO_NEW`，参数带清单的 `min_core_hint` |
| 发布者证书、信任更新、吊销列表 | `format` | 1 | `PLUGIN_SCHEMA_TOO_NEW` |
| 信封、握手、健康、错误、推送与确认 | 协议主版本 | 1 | 握手时 `PLUGIN_PROTOCOL_UNSUPPORTED` |
| 推送表的行 | 推送的 `schema`，即协商好的能力版本 `table.<kind>@<n>` | 1 | `PLUGIN_SCHEMA_TOO_NEW` |

- 协议主版本 1 冻结：信封、握手、错误与推送信封的形状不再变化。
- 能力（`routes`、`table.availability` 等）各自带版本；在主版本 1 内发布过的能力版本永不移除。
- 任何改变核心执行结果的字段都只能作为对象的新版本出现（Go 里是旧类型旁边的新类型），旧读者因此拒绝它，而不是悄悄丢掉一条限制。
- 已发布的黄金文件永不修改或删除（见[黄金文件](#黄金文件)）。

## 规范 JSON 与摘要

签名的载荷和推送状态的摘要都用同一种字节形式，所以一个签名或摘要只对应一份文档。规范 JSON 是 UTF-8 JSON，并且：

- 没有多余的空白；
- 对象成员按键的 UTF-8 字节排序，没有重复键；
- 只有整数，十进制书写，没有正号、前导零、小数或指数，零不带负号；
- 字符串只在 JSON 要求时转义：`\"`、`\\`，控制字符 U+0008、U+0009、U+000A、U+000C、U+000D 写成 `\b`、`\t`、`\n`、`\f`、`\r`，其余低于 U+0020 的控制字符写成小写十六进制的 `\u00xx`，其他字符都写成 UTF-8 字节本身。

摘要写作 `sha256:<小写十六进制>`，是规范字节的 SHA-256。

## 签名信封

签名的文档以信封传递：载荷字节与对它的签名放在同一个文件里，读者不会把一份文档与它另一个版本的签名配在一起。`manifest.sig`、`publisher.crt` 里的每张证书与信任更新都是信封。它与 server 发布通道的信封形状相同。

<!-- golden: envelope/v1/valid/two_signatures.json -->
```json
{"payload":"eyJjcmVhdGVkIjoiMjAyNi0xMS0xN1QwMDowMDowMFoiLCJmaWxlcyI6W3siZXhlYyI6dHJ1ZSwicGF0aCI6ImJpbi9saW51eF9hbWQ2NC9hY21lLWJhY2t1cCIsInNoYTI1NiI6IjUxYTFmMDVhZjg1ZTM0MmUzYzg0OWI0N2QzODcwODY0NzYyODJkNWY1MGRjMjQwYzE5MjE2ZDZlZGZiMWViNWEiLCJzaXplIjo0MDk2fV0sImlkIjoiYWNtZV9iYWNrdXAiLCJwcm90b2NvbCI6eyJtYXgiOjEsIm1pbiI6MX0sInB1Ymxpc2hlciI6ImFjbWUiLCJzY2hlbWEiOjEsInZlcnNpb24iOiIxLjAuMCJ9","signatures":[{"keyid":"acme-2026a","sig":"EREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREREQ=="},{"keyid":"acme-hsm-1","sig":"MEQiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIg=="}]}
```

| 字段 | 规则 |
|---|---|
| `payload` | 签名的字节原样（base64），必须是规范 JSON，至多 4 MiB |
| `signatures` | 1–8 个 `{keyid, sig}`；同一信封里 `keyid` 不重复；`sig` 为 1–512 字节 |

示例里的签名是填充字节，只演示形状。

算法属于受信任的密钥，不属于签名：读者用自己信任的、该 key id 下那把密钥的算法验证，签名无法选择更弱的算法。

| `alg` | 公钥 | 签名 |
|---|---|---|
| `ed25519` | 32 字节 Ed25519 公钥 | 对载荷的 64 字节纯 Ed25519 签名（RFC 8032） |
| `ecdsa-p256-sha256` | 未压缩 SEC 1 格式的 P-256 点（65 字节，`0x04‖X‖Y`） | 对载荷 SHA-256 的 ASN.1 DER 签名，即硬件密钥与云端密钥服务返回的形式 |

key id 符合 `^[a-z0-9][a-z0-9._-]{1,63}$`，永远只指一把密钥，从不复用。项目根密钥的 key id 以 `root-` 开头，证书里密钥的 key id 以 `<publisher>-` 开头（且不以 `root-` 开头），所以根密钥与发布者密钥、两个发布者的密钥永远不会同名。验证规则：

- 读者不认识的 key id 的签名被跳过，所以签名方可以先用新旧两把密钥一起签，再让读者改为信任新密钥。
- 受信任密钥的签名验证失败时，整个信封失败（`PLUGIN_SIGNATURE_INVALID`）：key id 只指一把密钥，失败只能是篡改或损坏，不可能是轮换。
- 至少要有一个受信任的签名通过，否则为 `PLUGIN_SIGNATURE_UNTRUSTED`；载荷不是规范 JSON 时为 `PLUGIN_WIRE_NOT_CANONICAL`。

## 包清单

`manifest.json` 是包的清单，按规范 JSON 存放，它的字节就是 `manifest.sig` 的载荷。`publisher.crt` 是 1–4 个证书信封组成的 JSON 数组，发布者轮换密钥时可以同时带新旧两张证书。

<!-- golden: manifest/v1/valid/minimal.json -->
```json
{"created":"2026-11-17T00:00:00Z","files":[{"exec":true,"path":"bin/linux_amd64/acme-backup","sha256":"51a1f05af85e342e3c849b47d387086476282d5f50dc240c19216d6edfb1eb5a","size":4096}],"id":"acme_backup","protocol":{"max":1,"min":1},"publisher":"acme","schema":1,"version":"1.0.0"}
```

用到全部字段的例子是语料里的 `manifest/v1/valid/full.json`。

| 字段 | 规则 |
|---|---|
| `schema` | 1 |
| `id` | 包 id，`^[a-z][a-z0-9_]{1,31}$`；也是路由段 `/api/v1/x/<id>/` 与目录名。第三方的 id 以 `<发布者>_` 开头，可用哪些前缀由证书授予 |
| `publisher` | `^[a-z][a-z0-9]{1,15}$`（不含下划线，`<发布者>_` 因而能无歧义地切开包 id）；必须等于签名证书的主体 |
| `version` | 语义化版本，不带前导 `v` |
| `min_core_hint` | 可选，至多 32 字节，只用于提示需要哪个 server 版本；兼容与否由 `requires` 决定 |
| `created` | 必须落在签名证书的签发窗口内 |
| `protocol` | 包支持的 `gwx` 主版本范围 `{min, max}` |
| `requires` | 包离不开的能力 → 需要的最低版本；server 缺任何一项即判定不兼容并说明是哪项 |
| `uses` | 可选能力 → 包能用的最高版本，运行时向下协商；同一能力不能同时出现在 `requires` 里 |
| `roles` | 包声明的特权角色，已排序；证书授予了才生效 |
| `permissions` | 管理员批准的权限 `<动词>:<对象>`，已排序；动词只有 `events`、`http`、`metrics`、`node`、`notify`、`push`、`read`、`svc` |
| `features` | 声明的功能键 `{key, presentation, gate}`：`presentation` 为 `promote`（显示为锁定）或 `conceal`（隐藏）；`gate` 为 `none`（总是完整）、`self`（本包作答）或 `authority`（只由命名空间的权威作答） |
| `routes` | 包的 HTTP 路由，见下表 |
| `ui` | 控制台界面包：`mode` 为 `module`（同源 ES 模块，需证书授予）或 `iframe`；`entry` 是 `ui/` 下的成员；`sdk` 是控制台 SDK 主版本范围；`nav` 是菜单项 |
| `node` | 节点部分：`app` 是 `node/` 下的应用描述，`images` 是 `node/images/` 下供离线站点用的镜像包 |
| `runtime` | 服务端部分：`lifecycle` 为 `resident`（随 server 启动）或 `on_demand`（用时启动、空闲停止，不拥有表）；`mem_mib`、`cpu_milli`、`max_concurrency`（≤1024）为 0 时取默认；`devices` 是 `/dev` 下的设备节点，已排序 |
| `storage` | 数据目录：`kind` 为 `sqlite` 或 `files`；`ha` 为 `shipped`（随 server 数据库复制）或 `leader_local`（可重建的缓存）；`state_version` 是本构建写的数据格式版本（≥1）；`cold_copy_safe` 表示插件停止时可以直接复制 |
| `host_setup` | 封闭词汇里的主机步骤，从不是脚本；目前只有 `usb_device`（`vendor_id`、`product_id` 各 4 位小写十六进制） |
| `codes` | 包可能产生的全部码，已排序；每个码在包的 locale 里有 en/zh 文案 |
| `files` | 除 `manifest.json`、`manifest.sig`、`publisher.crt` 外的每个成员，按路径排序：`{path, size, sha256, exec}`；路径是相对路径，每段以字母、数字或下划线开头（没有 `.`、`..` 与隐藏文件），至多 512 字节 |

清单的一个部分存在时，`requires` 必须列出对应的能力，这样不支持它的 server 按能力拒绝，而不是误处理：

| 部分 | 必须 `requires` |
|---|---|
| `routes` 非空 | `routes` |
| `ui` | `ui.bundle` |
| `node` | `node.app` |
| `host_setup` 非空 | `host.setup` |

路由挂在 `/api/v1/x/<id><pattern>`：

| 字段 | 规则 |
|---|---|
| `method` | `GET`、`POST`、`PUT`、`PATCH` 或 `DELETE`；`method` 加 `pattern` 不重复 |
| `pattern` | 以 `/` 开头，chi 语法，参数写作 `{name}` |
| `action` | `<包 id>.<动词>`，授权时用的动作名 |
| `access` | `admin`（管理员，会话或平台 token）、`admin_session`（仅管理员会话）、`member`（任何登录用户或平台 token）、`public`（不认证） |
| `feature` / `ungated` | 二者恰好设一个；`feature` 必须是本清单声明的键 |
| `write_when_read_only` | 功能只读时仍放行这个非 GET 写入（只做删除或放宽的写）；要求有 `feature` |
| `max_body` | 请求体上限，0 为 1 MiB，最大 1 GiB；超过 1 MiB 需要权限 `http:uploads` |
| `timeout_s` | 0 为 60 秒，最大 600 |
| `stream` / `websocket` | 流式响应 / WebSocket 升级（仅 GET，且不与 `stream` 同时） |
| `ai_readable` | AI 助手与 MCP 可读，仅 GET |
| `cookies` / `rate` | 仅 `public` 路由：允许设置 cookie（单点登录回调）/ 按客户端地址的令牌桶 `{per_minute, burst}`，各 1–100000 |

## 发布者证书

证书是发布者的签名密钥及其授权，作为信封的载荷传递，由项目根密钥签名（主机运维方信任的发布者按运维方的配置）。签发者是验证通过的签名的 key id，载荷里没有签发者字段。

<!-- golden: publisher_cert/v1/valid/authority.json -->
```json
{"alg":"ed25519","format":1,"grants":{"authority_id":"acme_quota","feature_ns":["acme"],"id_prefixes":["acme_"],"min_isolation":"landlock","roles":["availability_authority","clock_authority"],"tables":["fleet_capacity"]},"keyid":"acme-2026a","not_after":"2027-02-01T00:00:00Z","not_before":"2026-11-17T00:00:00Z","public_key":"1+5QRW1qr6d5EuAxQt77BRh28RH6+gBNDvAKPKdkHV4=","publisher":"acme","serial":17}
```

| 字段 | 规则 |
|---|---|
| `format` | 1 |
| `serial` | 在该发布者的全部证书里唯一（无论由哪把根密钥签发），所以 `publisher` + `serial` 指一张证书；≥1 |
| `publisher` | 主体，用这把密钥签的清单必须写同一个名字 |
| `keyid` / `alg` / `public_key` | 发布者的签名密钥，规则同[签名信封](#签名信封)；`keyid` 以 `<publisher>-` 开头，且不以 `root-` 开头 |
| `not_before` / `not_after` | 签发窗口；`not_after` 必须晚于 `not_before`。`created` 落在窗口内的包在 `not_after` 之后仍然有效，已安装的包不会过期 |
| `grants.id_prefixes` | 必填：这把密钥可签的包 id，已排序，每项符合包 id 文法。以 `_` 结尾的一项覆盖以它开头的所有 id，其他项只覆盖这一个 id。除项目发布者外，每项都以 `<publisher>_` 开头，所以证书永远覆盖不到别的发布者的 id（`acme_` 不覆盖 `acmebank_pay`） |
| `grants.feature_ns` | 包可以在其中声明功能键的命名空间，已排序：只能是发布者自己的名字，项目发布者另可有 `""`（无前缀命名空间） |
| `grants.authority_id` | 唯一能对 `feature_ns` 里 `gate: authority` 的键作答的包 id；要求有 `feature_ns`，且被 `id_prefixes` 覆盖 |
| `grants.roles` / `grants.tables` | 包可以声明的特权角色 / 可以推送的特权表，已排序 |
| `grants.ui_module` | 包可以提供同源的控制台模块 |
| `grants.min_isolation` | 必填：`landlock`（只在 Landlock 能约束它的主机上运行）或 `none` |

项目发布者名为 `gpuworks`，它自己的包用无前缀的功能键和不带发布者前缀的 id：

<!-- golden: publisher_cert/v1/valid/project.json -->
```json
{"alg":"ed25519","format":1,"grants":{"authority_id":"quota","feature_ns":["","gpuworks"],"id_prefixes":["quota","sso"],"min_isolation":"landlock","roles":["availability_authority","clock_authority"],"tables":["fleet_capacity"]},"keyid":"gpuworks-2026a","not_after":"2027-02-01T00:00:00Z","not_before":"2026-11-17T00:00:00Z","public_key":"0RZF/KSeB5I5emmRFX7vHLklBOUA2A8ot0mLncySqxs=","publisher":"gpuworks","serial":1}
```

## 信任更新与吊销列表

信任更新由读者当前信任的至少一把项目根密钥签名，可以离线导入。读者记住已应用的最高 `serial`，拒绝更低或相等的，所以旧的更新无法重放。

<!-- golden: trust_update/v1/valid/rotate.json -->
```json
{"add_roots":[{"alg":"ed25519","keyid":"root-3","public_key":"eC0hm7/hzMkrq452w7FbsvdumRZzFEroSPiJYIFGCqs="},{"alg":"ecdsa-p256-sha256","keyid":"root-4","public_key":"BMX+c4VTtfKGKA3l0EjO3d6IXwcBTVbSabaijGatYQCn+OSfRFHDO2UzUlWZu07cpYIEmdreHZH0eYnGPrSojuY="}],"created":"2026-12-01T00:00:00Z","floors":[{"id":"acme_backup","min_version":"1.1.0"},{"id":"acme_quota","min_version":"2.0.3"}],"format":1,"retire_roots":["root-1"],"revoke":[{"keyid":"acme-2025a"},{"cert_serial":9,"publisher":"acme"}],"serial":3}
```

| 字段 | 规则 |
|---|---|
| `format` / `serial` / `created` | 1 / 递增的序号 / 签发时间 |
| `add_roots` | 从此信任的根公钥，按 key id 排序，每个 key id 以 `root-` 开头；不能同时出现在 `retire_roots` |
| `retire_roots` | 不再信任的根 key id，已排序，每个以 `root-` 开头 |
| `revoke` | 吊销条目，见下文；至多 4096 条 |
| `floors` | 抬高包可安装的最低版本 `{id, min_version}`，按 id 排序；最低版本只升不降 |

四个列表至少有一个非空。一条吊销恰好用一种方式指明对象，同一列表里不重复：

| 选择方式 | 吊销什么 |
|---|---|
| `publisher` | 该发布者的全部证书 |
| `publisher` + `cert_serial` | 一张证书 |
| `keyid` | 一把密钥，无论在哪张证书里 |
| `manifest` | 一个包构建，按清单摘要；只有这种条目可以带 `replacement`（应改装的版本） |

按清单摘要而不是包文件的哈希吊销，重新压缩 tar 也逃不掉。

吊销列表是累积的：读者按签发者记住最高的 `serial`。项目根签发的列表不写 `publisher`；发布者也可以就自己的密钥与包构建签发列表，此时写上自己的名字，每条条目都只能关于它自己。`revoked` 不能为空。

<!-- golden: crl/v1/valid/root.json -->
```json
{"created":"2026-12-01T00:00:00Z","format":1,"revoked":[{"cert_serial":9,"publisher":"acme"},{"keyid":"acme-2025a"}],"serial":12}
```

## gwx/1 传输与握手

按 ADR 0071，插件是 server 的子进程，经两对继承的 socketpair 各跑一条明文 HTTP/2：插件在一条上为 server 提供服务，在另一条上调用宿主接口。以下常量随主版本 1 冻结在 `pkg/extapi` 里：

| 名称 | 值 | 作用 |
|---|---|---|
| 环境变量 `GWX_PROTOCOL` | 主版本 | 插件进程环境里的协议主版本 |
| 环境变量 `GWX_SERVE_FD` / `GWX_HOST_FD` | fd 编号 | 插件在其上提供服务 / 调用宿主接口的描述符 |
| `POST /gwx/v1/initialize` | 握手 | server 对插件的第一次调用 |
| `/gwx/v1/shutdown` | 停止 | 请插件退出 |
| `GET /gwx/v1/health` | 健康 | server 的周期探测 |
| `PUT /host/v1/state/{kind}` | 推送 | 插件推送一种表的整表 |
| 请求头 `Gwx-Deadline-Ms` | Unix 毫秒 | 调用的截止时间 |
| 响应头 `Gwx-Product-Error` | — | 标明插件路由的响应体是一个错误对象 |

握手请求：

<!-- golden: handshake_request/v1/valid/first_start.json -->
```json
{"clock":{"extclock":"2026-12-01T00:00:00Z","wall":"2026-12-01T00:00:00Z"},"core":{"arch":"amd64","ha_role":"leader","install_id":"inst_7f3a9c2e41b8","offer":{"host.events":{"max":1,"min":1},"host.fleet":{"max":1,"min":1},"routes":{"max":1,"min":1},"table.attestations":{"max":1,"min":1},"table.availability":{"max":1,"min":1},"table.banners":{"max":1,"min":1},"table.fleet_capacity":{"max":1,"min":1},"table.node_holds":{"max":1,"min":1}},"os":"linux","version":"v26.12.0"},"plugin":{"id":"acme_quota","permissions_granted":["http:routes","push:banners","read:fleet"],"version":"2.0.4"},"principal_key":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=","protocol":{"max":1,"min":1}}
```

| 字段 | 规则 |
|---|---|
| `protocol` | server 支持的主版本范围；不含 1 时为 `PLUGIN_PROTOCOL_UNSUPPORTED` |
| `core` | `version`、`install_id`、`os`、`arch`、`ha_role`（`leader` 或 `single`，插件只在主节点运行），以及 `offer`：server 提供的每种能力 → 版本范围，不能为空 |
| `plugin` | 管理员批准的内容：`id`、`version`、`permissions_granted`、`roles_granted`、`authority_namespaces`（本包作为权威的命名空间，`""` 为无前缀命名空间），列表都已排序 |
| `held` | server 为本包保存的每种表 `{kind, schema, generation, digest}`，`kind` 不重复；插件可以跳过摘要相同的推送 |
| `clock` | `wall` 墙钟与 `extclock`（时间线遵循的、不会倒退的时钟） |
| `principal_key` | 每次拉起新生成的 32 字节密钥，用来认证被代理路由上的主体请求头 |

插件的应答：

<!-- golden: handshake_response/v1/valid/initialized.json -->
```json
{"id":"acme_quota","manifest_sha256":"sha256:69a8fc5cf9fa0174e3dfe74b1c9c1f5f4f64710afa362419883f19c6b42ee422","protocol":1,"services":["backup","support"],"use":{"host.fleet":1,"table.attestations":1,"table.availability":1,"table.banners":1,"table.fleet_capacity":1},"version":"2.0.4"}
```

`protocol` 是插件从 server 的范围里选的主版本（目前只能是 1）；`manifest_sha256` 是插件构建时所带清单的摘要，server 据此拒绝不属于该包的可执行文件；`use` 是每种要用的能力 → 选定的版本，必须落在 server 的 `offer` 里；`services` 是插件提供的可选服务，已排序。

## 健康与错误

健康探测的应答：`status` 为 `ok` 或 `degraded`；`degraded` 必须带 `code` 说明原因，`params` 只能随 `code` 出现。

<!-- golden: health/v1/valid/degraded.json -->
```json
{"code":"ACME_QUOTA_STORE_LOCKED","params":{"path":"data/quota.db","retries":3},"status":"degraded"}
```

双向的每个 `gwx/1` 错误，以及插件路由返回的产品错误，都是同一个对象：

<!-- golden: error/v1/valid/busy.json -->
```json
{"code":"PLUGIN_BUSY","message":"the plugin is busy","reason":"busy","retry_after_ms":500}
```

| 字段 | 规则 |
|---|---|
| `code` / `params` | ADR 0067 的码与参数 |
| `message` | 必填的英文 fallback，至多 2048 字节 |
| `reason` | 与码无关的处理分类，主版本 1 内封闭：`invalid_argument`、`permission_denied`、`not_found`、`failed_precondition`、`conflict`、`busy`、`unavailable`、`unsupported`、`cancelled`、`deadline_exceeded`、`internal` |
| `retry_after_ms` | 建议的重试间隔，0–86400000，只用于 `busy` 与 `unavailable` |

## 推送与确认

插件把一种表的整表推给 server。所有者是这条连接绑定的包，推送里没有任何指明所有者的字段。整个推送编码后至多 16 MiB。

<!-- golden: state_push/v1/valid/empty_table.json -->
```json
{"body":{},"generation":1,"kind":"node_holds","schema":1}
```

| 字段 | 规则 |
|---|---|
| `kind` | 表的种类，与路径段相同 |
| `schema` | `body` 中行的版本，即插件协商的 `table.<kind>@<schema>` |
| `generation` | 按所有者与种类严格递增，≥1 |
| `base` | 只用于定义了补丁形式的种类；主版本 1 的种类都没有，所以必须缺省 |
| `body` | JSON 对象，按 `kind` 与 `schema` 严格解码 |

server 的确认要么表示已应用（`applied_at` 与 `digest`），要么表示提交之前被同一所有者同一种类更新的推送取代（`superseded_by`，大于 `generation`），二者恰好其一：

<!-- golden: state_ack/v1/valid/applied.json -->
```json
{"applied_at":"2026-12-01T00:00:00Z","digest":"sha256:b4267ce93cbba4e415504feb895f662dcced5d7aa406f27e8447c8fd5f0d48c8","generation":42,"kind":"availability"}
```

<!-- golden: state_ack/v1/valid/superseded.json -->
```json
{"generation":42,"kind":"availability","superseded_by":43}
```

每种表属于一个类别，决定 server 如何合并多个所有者的行：

| 类别 | 种类 | 合并 |
|---|---|---|
| 限制（Restrict） | `fleet_capacity`、`node_holds` | 任何获准的所有者都能推，按交集合并，所有者掉线时不失效 |
| 授权（Grant） | `availability` | 只来自命名空间的权威（`gate: self` 的键来自声明它的包），所有者掉线超过宽限后收紧 |
| 显示（Display） | `banners` | 只显示，从不参与执行 |
| 不透明（Opaque） | `attestations` | 只存储与转交，从不解释 |

## 推送表

行里的 `notice` 是一个码加参数，码必须列在推送方清单的 `codes` 里。时间线由步骤组成：只有第一步可以省略 `at`（从推送起生效），之后的 `at` 严格递增，每条时间线 1–64 步；server 按自己的时钟执行步骤，插件掉线时同样生效。

### `table.availability@1`

推送方作为权威（或以 `gate: self` 声明）的每个功能键的可用性时间线。

<!-- golden: availability/v1/valid/authority.json -->
```json
{"down_grace_s":3600,"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full","limits":[{"dim":"concurrent_jobs","max":4},{"dim":"gpus","unlimited":true}]},{"access":"full","at":"2027-01-01T00:00:00Z","link":"/x/acme_quota/renew","notice":{"code":"ACME_QUOTA_GRACE","params":{"days":30}}},{"access":"read_only","at":"2027-02-01T00:00:00Z","link":"https://acme.example/renew","notice":{"code":"FEATURE_EXPIRED"}}]},{"display":{"plan":"pro"},"down_access":"locked","feature":"acme.reports","steps":[{"access":"locked","notice":{"code":"ACME_NOT_PURCHASED"}}]}]}
```

- `down_grace_s`：所有者掉线后各行保持推送值的秒数；0 取 server 默认，否则在 60–86400 之间，站点只能调低。
- `rows`：至多 4096 行，`feature` 不重复；`down_access` 为 `read_only` 或 `locked`，是超过宽限后收紧到的上限。
- 步骤：`access` 为 `full`、`read_only`、`locked` 或 `hidden`；可选的 `notice`；`link` 为 https 地址或控制台路径（至多 2048 字节）；`limits` 至多 16 个、按 `dim` 排序，每个恰好设 `max` 与 `unlimited` 之一。

### `table.fleet_capacity@1`

集群容量上限：server 在节点加入时拒绝超限的加入，并把超限的节点保持在调度之外。推送方只给上限，选哪些节点由 server 决定。

<!-- golden: fleet_capacity/v1/valid/authority.json -->
```json
{"hold":{"code":"ACME_OVER_CAPACITY"},"refuse_card":{"code":"ACME_CARD_LIMIT","params":{"max":64}},"refuse_node":{"code":"ACME_NODE_LIMIT","params":{"max":8}},"select":"enrollment_order","steps":[{"max_cards":64,"max_nodes_with_cards":8},{"at":"2027-02-01T00:00:00Z","max_cards":16,"max_nodes_with_cards":2}]}
```

`select` 目前只有 `enrollment_order`（按加入顺序保留节点，满了以后保持其余节点）；`refuse_node`、`refuse_card` 是超限加入被拒时的说明，`hold` 是被保持节点上显示的原因；每一步的 `max_nodes_with_cards`、`max_cards` 缺省即不限。

### `table.node_holds@1`

推送方保持在调度之外的节点（维护、隔离）及各自的原因。保持从不停止运行中的工作；指向不存在节点的行会保留，但不匹配任何节点。

<!-- golden: node_holds/v1/valid/maintenance.json -->
```json
{"nodes":{"12":{"code":"ACME_MAINTENANCE","params":{"ticket":"OPS-118"}},"9":{"code":"ACME_QUARANTINE"}}}
```

`nodes` 的键是 server 的十进制节点 id（从不复用），至多 10000 个。

### `table.banners@1`

控制台每页顶部的横幅，至多 16 条。

<!-- golden: banners/v1/valid/authority.json -->
```json
{"banners":[{"audience":"admin","id":"grace","steps":[{"link":"/x/acme_quota","notice":{"code":"ACME_QUOTA_GRACE","params":{"days":30}},"severity":"warning"},{"at":"2027-02-01T00:00:00Z","hidden":true}]},{"audience":"all","display":{"icon":"key"},"id":"usb_key_missing","steps":[{"at":"2026-12-01T00:00:00Z","notice":{"code":"ACME_USB_KEY_MISSING"},"severity":"danger"}]}]}
```

`id` 由所有者自定，不重复；`audience` 为 `admin` 或 `all`；每一步要么 `hidden: true`（此时不带其他字段），要么带 `severity`（`info`、`warning`、`danger`）与 `notice`，可选 `link`。

### `table.attestations@1`

server 存储并转交给同一发布者其他包的不透明声明，server 从不解释。

<!-- golden: attestations/v1/valid/authority.json -->
```json
{"attestations":[{"blob":"c3RhdGVtZW50LTE=","feature":"acme.backup","format":"acme.grant-statement.v1","name":"grant_backup"},{"blob":"c3RhdGVtZW50LTI=","feature":"acme.reports","format":"acme.grant-statement.v1","name":"grant_reports"},{"blob":"AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+P0BBQkNERUZHSElKS0xNTk9QUVJTVFVWV1hZWltcXV5fYGFiY2RlZmdoaWprbG1ub3BxcnN0dXZ3eHl6e3x9fn+AgYKDhIWGh4iJiouMjY6PkJGSk5SVlpeYmZqbnJ2en6ChoqOkpaanqKmqq6ytrq+wsbKztLW2t7i5uru8vb6/wMHCw8TFxsfIycrLzM3Oz9DR0tPU1dbX2Nna29zd3t/g4eLj5OXm5+jp6uvs7e7v8PHy8/T19vf4+fr7/P3+/w==","format":"acme.binding.v1","name":"install_binding"}]}
```

至多 1024 条；`name` 不重复；`feature` 可选；`format` 告诉读者如何验证 `blob`；`blob` 非空，至多 64 KiB。

## 错误码

解码、校验与验证的每一种失败都是带稳定码与参数的错误，英文消息只是 fallback；控制台带全部码的 en/zh 文案。

| 码 | 含义 | 参数 |
|---|---|---|
| `PLUGIN_WIRE_MALFORMED` | 不是期望形状的一个 JSON 值 | `object`、`field`、`rule` |
| `PLUGIN_WIRE_UNKNOWN_FIELD` | 该版本未定义的字段 | `object`、`field` |
| `PLUGIN_WIRE_INVALID` | 字段违反对象的规则 | `object`、`field`、`rule` |
| `PLUGIN_WIRE_NOT_CANONICAL` | 签名的载荷不是规范 JSON | `object` |
| `PLUGIN_SCHEMA_TOO_NEW` | 对象的版本比本构建认识的新 | `object`、`version`、`known` |
| `PLUGIN_MANIFEST_TOO_NEW` | 清单的 schema 比本构建认识的新 | `version`、`known`、`min_core_hint` |
| `PLUGIN_PROTOCOL_UNSUPPORTED` | 对方的协议范围不含本构建的主版本 | `min`、`max`、`supported` |
| `PLUGIN_SIGNATURE_UNTRUSTED` | 没有受信任密钥的签名 | `object` |
| `PLUGIN_SIGNATURE_INVALID` | 受信任密钥的签名验证失败 | `object`、`keyid` |
| `PLUGIN_STATE_REJECTED` | 推送表的行违反该种类的规则 | `kind`、`field`、`rule`、`row` |
| `PLUGIN_STATE_TOO_LARGE` | 推送表超过该种类的大小上限 | `kind`、`field`、`max` |
| `PLUGIN_STATE_TOO_NEW` | 持久化状态的版本标记比本构建认识的新 | `marker`、`version`、`known`、`needs`、`count` |

`rule` 取值：`required`（缺失或为空）、`grammar`（不符合文法）、`range`（数值或长度越界）、`enum`（不在封闭集合内）、`duplicate`、`order`（应排序的列表未排序）、`exclusive`（设了互斥的字段）、`relation`（与同一对象的另一字段矛盾）、`syntax`、`type`、`number`（非整数或越出安全范围）、`trailing`。`field` 是点分路径，列表元素用下标，例如 `routes.0.feature`。

`PLUGIN_STATE_TOO_NEW` 由 `extapi.CheckMarkers` 给出：比较持久化的版本标记（名字、版本、写入它的 server 版本）与本构建能读的最高版本，`needs` 是这些标记的写入者里最新的 server 版本，即应改为运行的版本。

## 黄金文件

`pkg/extapi/testdata/golden/<种类>/v1/` 是冻结的线上语料：

- `valid/` 下的每个文件都能解码、通过校验，并重新编码成相同的字节；
- `invalid/<码>/` 下的每个文件都恰好以该码失败；文件名 `<字段>--<规则>[--说明].json` 同时给出失败的字段与规则，`_root` 表示对象本身；
- `testdata/golden/SHA256SUMS` 只能追加：已发布的黄金文件永不修改或删除，`make check-architecture` 与基线比对强制这一点；对象的新版本放进自己的 `v2/` 目录。

在仓库根目录用 `make test-extapi` 跑这个模块的格式检查、vet 与测试，`make test` 包含它。
