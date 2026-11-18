# 扩展点：模块契约与可安装扩展

gpuworks 的 server 在核心内部经一组接口组合模块（`app.Module` 与 `app.Host`）。这套契约今天由测试与分叉使用：另一个 Go 模块可以把 server 当作库，加上自己的模块编成新的 server 二进制（下文称「单独构建的 server」）；官方构建不带任何模块。官方与第三方扩展以签名插件包的形式安装，由内置的插件宿主模块经同一套契约接入（[ADR 0071](../adr/0071-installable-extensions.md)）；插件宿主已实现的部分与尚未实现的部分见[可安装扩展](#可安装扩展)。

本页说明开源代码留下的接口、每个接口在哪个线程上被调用、必须遵守的规则，以及开源代码不做的事。不可逆的取舍见 [ADR 0070](../adr/0070-extension-tiers-and-edition-boundary.md) 与 ADR 0071；术语（扩展模块、功能键、可用性、扩展停止调度、插件包、推送表）以 `CONTEXT.md` 为准。

扩展相关的文档、示例与测试都按机制本身描述；示例里的发布者、版本名与插件统一用虚构的 `acme`，就像示例域名用 `example.com`。

## 层级

| 层级 | 运行在哪里 | 放什么 |
|---|---|---|
| T0 核心 | server、agent | 全部通用能力与每个执行点：逐请求、逐插入、逐状态上报、逐认领、逐放置的判断都在这里；社区版本身，按定义免费 |
| T1 内置模块 | server 进程内，Go | 经 `app.Module` / `app.Host` 的核心内部组合；`cli.Options.Extensions` 留给测试与分叉，不是厂商或第三方的分发途径 |
| T2 插件包的服务端部分 | server 的子进程 | 需要 I/O、状态、流程或对接的决定，把结果推成核心执行的表（安装、监管与推送状态已实现，见[可安装扩展](#可安装扩展)；其余见[尚未实现](#尚未实现)） |
| T3 插件包的节点部分 | 节点上的核心工作负载与作业 | 用 GPU 的产品，agent 里不跑插件代码（尚未实现） |
| T4 策略部分 | server 内 | 只预留，不实现 |

本页的接口都属于 T1。T2–T4 的设计见 ADR 0071。

## 组合方式

单独构建的 server（分叉或测试）有自己的 `main`，只调用一次 `cli.Main`：

```go
func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Options{
		Edition:      "acme",
		Extensions:   app.Extensions{Modules: []app.Module{audit.New(), sso.New()}, UI: console.FS()},
		Distribution: distribution.Profile{Edition: "acme", Sources: sources, Keys: keys},
		InstallHooks: []selfinstall.InstallHook{devices.Hook{}},
		Commands:     []cli.Command{statusCommand},
	}))
}
```

`cli.Options` 的零值就是 `cmd/gpuworks` 的社区版 server：

- `Edition` 写入 `version.Edition`，只用于显示：`gpuworks edition`、`/healthz`、构建信息、用量报告和控制台清单都读它。开源代码从不按它分支（`edition_branches` 预算为 0）。
- `Extensions.Modules` 交给 `serve`；`migrate`、`reset-password`、`support-bundle` 与追加子命令打开数据库时也带上模块的迁移（追加子命令用 `cli.Env.OpenMaintenanceStore`）。
- `Extensions.UI` 替换内嵌的社区版控制台，布局与 `internal/server/webui/dist` 相同；为空时服务内嵌构建。
- `Distribution` 是该构建自己的发布通道：版本检查、通知、目录更新与升级都读它，签名索引里的版本名不符即拒绝。它的版本名必须与 `Edition` 相同，否则启动即 panic。
- `InstallHooks` 在 `install`、`upgrade` 时依次执行，`uninstall` 时倒序执行，用于设备权限之类必须由 root 完成的主机步骤。

官方构建传零值。`Edition` 与 `Distribution` 的按版本名作用域只服务分叉构建，官方发布流程不再扩展它们（ADR 0071）。

## 模块与 Host

```go
type Module interface{ Name() string }                     // [a-z][a-z0-9_]{1,31}
type Migrator interface{ Migrations() store.MigrationSet } // optional
type Initializer interface{ Init(ctx context.Context, h *app.Host) error }
type Service interface{ Run(ctx context.Context) error }
```

模块名是它唯一的标识：`server.yaml` 里 `extensions.<名>` 一节、迁移命名空间、日志的 `module` 标签都用它。名字非法、重复，或迁移集的命名空间不是模块名，server 在打开数据库之前就拒绝启动。

`Init` 在 `app.New` 里、API 依赖构建完之后、路由构建之前按模块顺序执行，只执行一次。模块只能经 `Host` 接触 server：

| 方法 | 作用 |
|---|---|
| `Log()` / `Bus()` / `DataDir()` / `InstallID()` | 带 `module=<名>` 的日志、事件总线、数据目录、安装标识 |
| `Section(v)` | 严格解码 `server.yaml` 的 `extensions.<名>`；没有这一节时 `v` 保持原样 |
| `Tables()` | 本模块的表（`store.ExtensionTables`）；没有迁移集的模块为 nil |
| `DeriveKey(purpose)` | 由 master key 经 HKDF-SHA256 派生、与模块名绑定的 32 字节密钥 |
| `MountAPI(e)` | 把 `api.Extension` 的路由挂到 `/api/v1/x/<名>/` |
| `DeclareFeatures(...)` / `ProvideFeatures(src)` | 声明功能键；为编译模块声明的功能键提供功能来源（全部模块中至多一个；插件包声明的功能键由插件宿主作答，不经它） |
| `AddBannerSource(b)` | 控制台每页顶部的横幅 |
| `GuardNodeInsert(g)` | 新节点插入时的守卫 |
| `HoldScheduling(h)` | 调度保持者，按来源名区分，来源重复即拒绝 |
| `ReapplySchedulingHolds()` | 保持者的答案变了：请求对全部节点重新应用一次保持，不等各节点的下次状态上报 |
| `NodeFleet(ctx)` | 全部节点的保持事实（`store.NodeHoldFacts`），按 id（加入顺序）排序，读池上一次查询 |
| `NodeIdentity(ctx, ids, salt)` | 指定节点的加盐硬件身份（`store.NodeIdentity`），原始序列号不出核心，见[节点身份与节点清单](#节点身份与节点清单) |
| `NodeInventory(ctx, cursor, limit)` | 按加入顺序分页的节点清单（`store.NodeInventoryPage`）：名称、加入时间、自己的卡（序号、型号、有无序列号） |
| `NodeNamesByID(ctx, ids)` | 指定节点的显示名，以 id 为键（`map[uint]string`），读池上一次查询 |

`Host` 不交出 `*store.Store`，也不交出数据库句柄。`Init` 返回之后再注册会被拒绝（`ReapplySchedulingHolds`、`NodeFleet`、`NodeIdentity`、`NodeInventory` 与 `NodeNamesByID` 不是注册，任何时候都可调用）：有返回值的方法返回 `app.ErrHostClosed`，其他方法 panic。`Init` 出错时 `New` 失败并关闭数据库，同一数据目录可以立即再次启动。

扩展路由挂在 `/api/v1/x/<名>/` 下：`api.RouteSpec.Pattern` 相对 `/api/v1`，是 `/x/<名>` 或以 `/x/<名>/` 开头，且是规整的路径（没有 `.`、`..`、`//` 与结尾的 `/`）；越出自己的命名空间、路径不规整，或同一方法下两个模式只差参数名（`{id}` 与 `{name}`），都在路由构建时 panic。核心不在 `/api/v1/x/` 下注册任何路由，所以模块名可以与核心路径段同名（如 `nodes`），也不会与核心将来新增的路径冲突。

所有 `Init` 结束后，server 先释放本构建没有任何模块认领的扩展停止调度（换了构建或去掉了模块时，它们不会一直留着），有保持者时再对全部节点应用一次保持，然后构建功能门（`featuregate.New`）并交给 API。

## 保持者的重新应用与节点集合

保持者的答案平时在每次状态上报时被问到，所以答案变了的节点最多要等一分钟才被停止或恢复调度。答案变了（例如快照重建后被保持的集合不同了）的模块调用 `ReapplySchedulingHolds()`，server 对全部节点再执行一次 `ApplySchedulingHolds`：

- 不阻塞、不分配内存，可以在任何 goroutine 调用，**唯独不能在钩子里**（`SchedulingHolder.Hold`、插入守卫）：一次重新应用会再问一遍所有保持者，在 `Hold` 里请求就会让全部节点不停地重新应用。钩子里同样不请求任何其他重算。
- 请求会合并：一次重新应用等待或执行期间的任意多次请求，最多再引起一次；两次之间至少隔 2 秒，所以答案来回抖动的保持者每 2 秒最多花一次全表写事务，不会变成每个事件一次。
- 重新应用在模块的 `Service` 所在的同一个 `services()` 循环里执行，不占调用方的 goroutine，随 server 停止；失败时记错误日志，从 2 秒起加倍退避（最长一分钟）重试。没有注册保持者时什么也不做。

答案取决于整个节点集合的保持者（例如按加入顺序挑选节点），用下面两样了解集合，而不是在 `Hold` 里记下见过的节点：

- `NodeFleet(ctx)`：全部节点的保持事实，读池上一次查询。只用于启动时和节点集合变化之后合并执行的重算，不在每次上报或每个事件里调用，也不在钩子里调用。先订阅下面的主题再第一次读取，二者之间的变化就不会漏掉。
- 总线主题 `store.NodeFleetTopic`：节点插入（三条插入路径）、删除，以及改变了保持事实（卡数或厂商；id、UUID、加入时间与所属 Kubernetes 集群不会变）的状态上报，各发一个事件，以节点 id 为键；用 `store.NodeFleetChangeOf(ev)` 读出 `{NodeID, Kind}`（`NodeInserted`、`NodeDeleted`、`NodeFactsChanged`），不要解析 `node` 主题的原因文本。没有改变保持事实的例行上报不发事件，所以发布方的开销不变。和所有总线主题一样，跟不上的订阅者只收到每个节点最新的一个事件，所以订阅者把任何事件都当作「这个节点、因而集合变了」，合并后从 `NodeFleet` 重算，不数事件。节点 id 删除后不再复用。

`Service.Run` 由 `App.Run` 的同一个 `services()` 循环启动，与健康检查、卡故障记录、用量报告、通知刷新并列，在控制器启动之后运行；`ctx` 结束时返回，返回的错误只记日志、不重启。同一个 App 可以多次 `Run`（原地重启），所以 `Run` 必须能被再次调用。

## 节点身份与节点清单

需要认出「同一台机器、同一批卡」的模块（如节点清单导出、支持包）经 `Host` 读加盐摘要，而不是原始序列号：

- **主机系统 UUID**：agent 在每次握手的 hello 里上报 `host_system_uuid`，即固件的系统 UUID（Linux 读 `/sys/class/dmi/id/product_uuid`，macOS 读 `IOPlatformUUID`），经 `proto.NormalizeHostSystemUUID` 规整（小写；占位值、全 0、全 F 与格式不对的值丢弃）。Linux 上该文件只有 root 能读，所以非 root 的 agent 不上报；旧 agent 也不上报，照常接入。server 收到后再规整一次，只在与节点行不同时写入 `nodes.host_system_uuid`，例行重连不多一次写；空值从不清掉已记下的值。它描述机器而不是节点：同一台机器上的多个逻辑节点上报同一个值。
- **卡的序列号**：卡的标识就是探测上报的 `GPUInfo.UUID`。`accelerator.CardSerial` 区分硬件自带、换槽换机也不变的序列号与探测按槽位或序号编出来的标识，只有前者参与身份：

  | 厂商 | 上报的标识 | 稳定 |
  |---|---|---|
  | NVIDIA、PPU、昆仑芯 | 厂商 CLI 给出的卡 UUID | 是 |
  | AMD | `amd-<unique_id>`（ASIC 序列号） | 是；驱动没有 `unique_id` 时为 `amd-<PCI 地址>`，否 |
  | 海光 | `GPU-<unique id>`（hy-smi 或 sysfs） | 是；没有时为 `hygon-<PCI 地址>` 或 `hygon-<序号>`，否 |
  | 昇腾 | `ascend-<序号>` | 否（npu-smi 的表格不给序列号） |
  | Apple | `APPLE-<IOPlatformUUID>` | 是，与主机系统 UUID 是同一台机器 |
  | 假探测器 | `GPU-FAKE-<n>`、`FAKE-<厂商>-<n>` | 否（测试值，每个节点都一样） |

- `NodeIdentity(ctx, ids, salt)` 对每个仍存在的指定节点给出 `{NodeID, Host, Cards, CardsWithoutSerial}`，按 id 升序：
  - `Host` 与 `Cards` 的每一项是 `store.IdentityPart{H, Tail}`。`H` 是以调用方的盐为密钥、对「种类、一个零字节、原始值」做 HMAC-SHA256 的前 16 个十六进制字符（8 字节），种类是 `host` 或 `card`；`Tail` 是原始值的最后 4 个字符，供人到机器上核对，原始值不足 8 个字符时为空，所以尾号永远不等于整个值。同一个盐每次得到相同的摘要，不同的盐得到互不相关的 `H`；`Tail` 不加盐，每个调用方看到的都是同一段原始尾号。
  - `Cards` 只含本节点可用（`accelerator.CardUsable`）且带序列号的卡，按 `H` 排序；可用但标识是编出来的卡只计入 `CardsWithoutSerial`。所以同一台机器上的两个逻辑节点共用 `Host`，各自只有自己的卡。
  - 部分答复：agent 还没上报主机 UUID 时 `Host` 是零值（`Known()` 为假）；不存在的节点（从未有过或已删除）不出现在结果里，调用方比对 id；重复的 id 只算一次。
  - `ids` 为 1 到 500 个节点，否则返回 `store.ErrNodeReadBatch`；盐为 1 到 256 字节，否则返回 `store.ErrIdentitySalt`。越界的调用被拒绝，不会被截断。
- `NodeInventory(ctx, cursor, limit)` 按 id（加入顺序）返回一页 `store.NodeInventoryPage{Nodes, Next}`：每个节点的 id、UUID、显示名（运维起的名字，否则主机名）、加入时间、`HostReported`（主机 UUID 是否已知），以及本节点可用的卡 `{Index, Model, Serial}`，不含任何原始序列号。第一页 `cursor` 为 0，之后传上一页的 `Next`；`Next` 为 0 表示已是最后一页。`limit` 为 1 到 500，否则 `store.ErrNodeReadBatch`。节点 id 不复用，所以翻页之间加入或删除的节点不会让其余节点移位或重复。
- `NodeNamesByID(ctx, ids)` 返回每个仍存在的指定节点的显示名（运维起的名字，否则主机名），以 id 为键；不存在的节点（从未有过或已删除）不在结果里，重复的 id 只算一次。只为少数节点打标签时用它，不必翻 `NodeInventory`。`ids` 为空时返回空 map、不查询；去重后超过 500 个返回 `store.ErrNodeReadBatch`，不会被截断。它是读池上一次按主键的查询。
- 规模：`NodeIdentity` 与 `NodeInventory`都是钉住的读，在读池上各两次带索引的查询（节点按主键查找或按主键范围、卡走 `gpus` 的 `(node_id, ident)` 索引），一次最多 500 个节点。按页面上显示的节点调用 `NodeIdentity`，不要在每个事件里对整个节点集合调用。身份的变化（新的主机 UUID、换卡）不在 `store.NodeFleetTopic` 上发布；需要跟上它的模块在读页面时重新读。

## 表与迁移

扩展迁移只服务编译进来的模块（分叉与测试）；插件包将用自己数据目录里的存储，核心为插件持久化的表是核心表，都不用这套机制（ADR 0071）。

迁移集（`store.MigrationSet`）在核心迁移之后、同一次 `store.OpenWith` 里执行：

- 每步与它的标记在同一事务里写入 `namespace_migrations`；该表只在传入迁移集时创建，社区版打开从不创建它，也不写 schema。
- 一步建立、修改或删除的表、索引、视图、触发器，名字和所属表都必须带该集的前缀；核心表从不使用保留前缀 `ee_`。
- 数据库里有本构建不认识的命名空间时只告警，不阻止启动；已知命名空间的版本比二进制新时拒绝启动；已记录的一步在二进制里缺失或改了名，同样拒绝。
- 核心的全表清理可能触及扩展表，所以扩展表一律存 UTC 时间，迁移只做加法。

`ExtensionTables.Write` 在唯一的写连接上开一个事务，`Read` 在读连接池上开一个只读事务（内存数据库没有读池，在写连接上读）。扩展只读写自己前缀的表，核心数据只经 Store 的方法、API 与下面的钩子。

## 钩子与调用线程

| 钩子 | 在哪里被调用 | 频率 |
|---|---|---|
| `NodeEnrollmentGuard` | 三条节点插入路径（`CreateNodePending`、`EnrollKubeNode`、`RegisterNode`）的写事务内 | 每次插入新节点 |
| `SchedulingHolder.Hold` | 每次状态上报与调度变更的写事务内；启动时与每次 `ApplySchedulingHolds`（含模块请求的重新应用）时对全部节点 | 每节点每次上报 |
| `featuregate.Source.Availability` | 扩展路由的每个请求、每次控制台清单请求 | 每请求 |
| `BannerSource.Banners` | 每次控制台清单请求 | 每请求 |
| `Service.Run` | 后台循环 | 常驻 |

插入守卫拒绝时，API 以 403 返回守卫的码；租机与 provider 安装路径会先做一次不占写锁的预检查，不会先租机再拒绝。保持者保持的节点停止调度、从不驱逐，运维不能恢复调度（409，带保持者的码），但仍可停止调度或驱逐。

## 纯函数规则

写连接只有一个（`store/database_open.go` 里 `SetMaxOpenConns(1)`）。在写事务里被调用的钩子（插入守卫、调度保持者）如果调用 Store、`Tables()` 或任何等待它们的东西，就会等自己持有的连接，形成死锁。所以这些钩子必须：

- 不调用 Store、`Tables()`，不做任何 I/O，不取有竞争的锁；
- O(1)：一次原子读加一次 map 查找；不保持时不分配内存；
- 从模块自己维护的快照作答，快照在别处（`Service`、事件订阅、API 写入）更新，快照变了再调用 `ReapplySchedulingHolds`；
- 拒绝时返回预先构造好的 `*messages.ProductError`。

功能来源与横幅来源在请求线程上被调用，同样从快照作答，不扫节点、部署或表。`testenv/extmodule` 里的契约测试演示了违反规则的后果：一个在守卫里写自己表的守卫，在插入事务结束前无法继续。

## 可用性

开源代码只说可用性：功能来源对每个功能键给出完整、只读、锁定或隐藏，可附限额、链接与带码的说明。

- 扩展路由（`api.RouteSpec`）要么指明 `Feature`，要么标记 `Community`，二者必居其一；路由清单（`api.RouteInventory`）打印每条扩展路由的访问级别、动作与功能。
- 完整：放行。只读：放行读，以及标记 `WriteWhenReadOnly` 的写（只做删除或放宽的写）；其他写返回 403 `FEATURE_READ_ONLY`。锁定：403 `FEATURE_UNAVAILABLE`。隐藏，或声明为隐藏呈现的功能被锁定：404。功能来源给了自己的说明时，返回它的码。
- 没有模块提供功能来源时，所有声明过的功能都是锁定（`FEATURE_PROVIDER_ABSENT`）。
- 社区路径从不查询功能门；正在运行的负载从不因可用性变化而中断。
- 扩展 GET 路由默认不给 AI 助手与 MCP 读，除非标记 `AIReadable`。

控制台按 `GET /api/v1/console/manifest` 渲染（字段见 [API 参考](../reference/api.md)），自己不推导可用性：完整照常显示；只读时组件收到 `readOnly`；锁定时菜单项带锁，页面显示锁定页和服务端给的链接；隐藏或清单里没有的功能键，什么都不显示。

## 控制台叠加

构建时叠加只服务分叉构建；插件包的界面将在运行时加载（ADR 0071，尚未实现）。

单独构建的 server 的界面模块复制到 `ui/src/extensions/<id>/`（已被 git 忽略），在 `ui/src/extensions/index.ts` 注册，只能 import `ui/src/extensions/sdk.ts` 再导出的组件、hook 与 API 客户端（`ee_ui_internal_imports` 预算为 0）。构建时设置 `GPUWORKS_UI_OUT_DIR` 输出到别处；检出了界面模块却没设这个变量时，`make ui` 与 `make release-packages` 拒绝构建，以免叠加进社区版。界面模块的文案放在自己的 locale 包里合并进 `common`，用 `GPUWORKS_I18N_EXTRA_LOCALES` / `GPUWORKS_I18N_EXTRA_MESSAGES` 跑同一套 i18n 检查。

界面模块能放的入口：

- 页面（`routes`）、侧边菜单项（`navItems`，必须带图标）、设置页签（`settingsTabs`）、顶栏控件（`headerItems`）。
- 账户菜单项：`navItems` 的 `group` 写保留值 `account`，入口进右上角账户菜单，排在内置项之后、退出登录之前，按扩展 id 排序，只显示文字。账户菜单没有放锁定入口的位置，锁定与隐藏一样不显示。`account` 不能再用作新菜单分组的 id。
- 仅管理员：以上每种入口都可标 `admin: true`，按会话检查时读到的用户角色显示。非管理员看不到菜单项、页签与控件，打开页面得到不存在页；读不到用户时同样按非管理员处理。这只影响显示，接口仍由服务端路由的访问级别把关。

横幅：链接指向当前页面（不计查询与片段）的横幅，在那个页面上不显示，由页面自己说明。

SDK 带主版本号 `SDK_MAJOR`（当前为 1），只在删掉或改变某个导出的含义时加一；新增导出不改版本，`ui/tests/consoleExtensionsSdk.test.ts` 固定导出清单。文件收发用 SDK 的 `downloadFile`、`saveBlob` 与 `sendForm`：

- `downloadFile` 带会话请求，文件名取响应的 `Content-Disposition`，没有时用调用方给的名字。错误响应按码解成本地化的 `APIError`，不会把错误正文存成文件。
- `saveBlob` 把一个 Blob 交给浏览器下载。
- `sendForm` 发 multipart 表单，错误解码与 `sendJSON` 相同。

界面模块的测试复制到 `ui/tests/ext-<id>/`（已被 git 忽略），用 `UI_EXTRA_TESTS='tests/ext-<id>/*.test.tsx' npm test` 跑。测试不在 `ui/src/extensions/<id>/` 里，所以 import `node:test`、testing-library 不计入 import 预算；`npm run test:coverage` 的 `c8 --all --include='src/**'` 仍统计界面模块的全部源文件。

## 码与文案

扩展产生的失败与拒绝同样遵守 [ADR 0067](../adr/0067-runtime-outcomes-as-codes.md)：UPPER_SNAKE 码、插值参数、英文 fallback。扩展的码与 en/zh 文案放在扩展自己的代码与界面模块里，不进开源的 `messages` 与 locale 文件；没有叠加界面时，控制台显示英文 fallback。

## 升级与安装

以下按版本名的检查只对分叉构建有意义：官方构建不写版本名，ADR 0071 决定第一个可用性权威作为插件运行之后从官方发布流程删除这套机制。

- 发布索引在签名内容里写明版本名；索引的版本名与构建的发布通道不符即拒绝，社区版只接受不写版本名或写 `community` 的索引。
- `upgrade` 比较已安装二进制与待安装二进制的 `gpuworks edition`，不同则拒绝，除非带 `--allow-edition-change`；控制台升级遇到版本不同返回 409 `SERVER_EDITION_MISMATCH`。
- `upgrade` 执行的是当前二进制的安装钩子；跨版本升级后按提示用新二进制再执行一次 `install`。
- 从扩展构建回到社区版时，`server.yaml` 里的 `extensions:` 节只产生告警；扩展的表原样保留；扩展停止调度在启动时释放。

## 可安装扩展

ADR 0071 决定官方与第三方扩展以签名插件包（`.gwpkg`）安装，而不是编进二进制。插件宿主已经作为内置模块接入本页的契约：

- 插件宿主是内置模块 `plugins`（`app/plugins.go`，实现在 `internal/server/pluginbridge`），经 `Host` 注册已有的钩子：节点插入守卫、调度保持者（来源 `plugins`）与横幅。已安装的包声明的功能键进功能门的动态集合，由插件推送、核心校验并持久化的表作答；编译模块声明的功能键仍由 `ProvideFeatures` 的来源作答，两者并存。编译模块不能再叫 `plugins`。
- 运行时功能声明：安装、升级、启用、停用或卸载一个包之后，动态集合整体替换为当时已安装的包声明的功能键，功能门与控制台清单立即看到，并在 `feature` 主题上通知控制台重读清单；不用重启。功能门查找先查编译模块声明的键，再查动态集合（一次原子读取，不加锁、不分配）；`GET /api/v1/console/manifest` 的 `features` 是两者合并后按键排序的列表。什么都没装时动态集合为空，清单与原来逐字节相同。
- 一个功能键只有一个归属：包声明的键已由编译模块声明，或已由另一个已安装的包声明（同一个包升级时保留自己的键）时，安装在要求批准之前就以 409 `PLUGIN_FEATURE_TAKEN{id, feature, owner}` 拒绝，`owner` 是模块名或那个包的 id，什么都不装。在这项检查之前装上的冲突包启动时照常加载：键留给原来的归属（编译模块，或 id 较小、自两者都安装起就作答的包），每处冲突记一条日志，归属不变；这样的包在下一个版本去掉冲突的键之前不能重装或升级。已安装但不再通过校验的包不占用它声明的键：它坏着的时候，别的包可以装上这个键；它恢复校验后，键归 id 较小的一方。
- 插件是 server 的子进程，经继承的 fd 用 `gwx/1` 与核心通信，从不进入热路径，也不拿 Store。
- 功能键是否受控由插件包自己声明；授权类的表只接受发布者证书指定的权威推送；插件掉线时限制继续执行，授权在有界宽限后降为只读，时间线按 extclock 照常生效。
- 本页的钩子规则（写事务里的钩子是纯函数、保持者只停止调度不驱逐、运行中的负载不中断）对插件宿主同样适用。

包清单、证书、信任更新、`gwx/1` 消息与推送表的线上格式冻结在 `pkg/extapi` 里，见[插件协议与线上格式](../reference/plugin-protocol.md)；包的读取、校验、信任与 `gpuworks plugin` 子命令见 [CLI 参考](../reference/cli.md#gpuworks-plugin)与[安装布局](../reference/install-layout.md)。

### 插件宿主模块

启动顺序（`plugins` 模块的 `Init`，在路由建好之前）：

1. 清掉中断的安装留下的目录，逐个重新校验已安装的包（这次全部重新哈希）；不再通过校验或已被吊销的包记为已安装但未校验：它推送的限制与横幅照常执行，授权被丢弃，它不会启动。
2. 用这份包清单读入全部推送表（见下文「推送状态」），在 server 接受第一个请求之前、任何插件启动之前生效。
3. 把已安装的包声明的功能键放进动态集合（与编译模块的键冲突时记日志并留给模块），注册钩子。

模块的服务（随 server 的其他后台循环运行）先启动推送状态的写入 tick、时间线与容量选择，再并行启动每个已启用、有 `resident` 服务端部分的包，不阻塞 `serve`；启动前只重新哈希变过的文件。插件的进程状态经监管回调推送状态：`running`、`degraded` 算在线，其余都算掉线。server 停止时先停插件，再停推送状态，所以每个插件掉线的时刻都被持久化，重启不会续期宽限。

推送方身份来自启动它时校验通过的包：监管为每个进程编号（全局递增的代数），宿主接口的推送按这条连接所属的包认定，带证书授予（含 `server.yaml` 为主机信任的签发者配置的权威）、管理员批准的权限、清单的码和这个进程的代数；连接所属的包已被替换或换了版本时推送被拒（`PLUGIN_STATE_CONNECTION_REPLACED`）。核心提供首批五种表的能力 `table.<种类>@1`，包的 `requires` 据此校验，插件握手时也据此协商。

包声明了 `storage` 时，它的数据带一个版本标记 `plugin.<id>.data`（在核心的 `plugin_markers` 表里），防止回退到读不了这份数据的旧构建：

- **写入**：每次启动后插件第一次进入 `running`（握手通过）时，宿主把标记写成 `max(已存的版本, storage.state_version)`，并记下运行的插件构建版本（被拒的旧构建据此指明要装的版本）；同一次启动里之后再进入 `running` 不再读写。标记从不调低：仍在运行的旧构建不会把它改小。没有 `storage` 的包不写标记，已经卸载的包也不再写。写入失败，或插件目录正被安装、卸载占用时，只记日志，下次健康检查时重试；读不到标记时不启动，稍后自动重试启动。
- **检查**：标记比包的 `storage.state_version`（没有 `storage` 时为 0）新时，安装这个构建以 `PLUGIN_STATE_TOO_NEW` 拒绝，什么都不装；已安装的构建（例如更新的构建运行过之后又装回旧构建）每次启动前也检查一次，不启动，列表里显示 `failed` 并带 `PLUGIN_STATE_TOO_NEW`，直到装上 `state_version` 足够新的构建。降级之前可以用 `gpuworks plugin compat` 做同样的检查。
- **生命周期**：普通卸载保留数据，也保留标记，所以重装旧构建同样被拒；只有 `purge` 在把数据目录移进 `.trash/` 的同时删掉标记。

### 管理接口

`/api/v1/plugins` 是核心路由，不受功能门控，插件全部掉线、崩溃循环或被卸载时照常可用；助手与 MCP 读不到（不在助手可读的路由里，处理函数也拒绝助手的进程内读取）：

| 方法与路径 | 谁可以调用 | 作用 |
|---|---|---|
| `GET /api/v1/plugins` | 管理员（会话或令牌） | 已安装的包与各自的状态、原因、可做的动作（服务端计算） |
| `POST /api/v1/plugins/install?approve=<批准值>&enable=true` | 管理员登录会话 | 请求体就是 `.gwpkg`；不带或带错批准值时回 409 `PLUGIN_APPROVAL_REQUIRED`，参数里给出批准值和它涵盖的权限、角色，什么都不装 |
| `POST /api/v1/plugins/{id}/enable`、`/disable` | 管理员登录会话 | 启用并启动；停止并不再启动（推送的表照常执行，按掉线计） |
| `DELETE /api/v1/plugins/{id}?purge=true` | 管理员登录会话 | 停止、卸载（`purge` 连数据一起移到 `.trash/`），在一次提交里删掉它推送的全部内容并重新应用保持；重试能完成一半失败的卸载 |

每次调用以 `audit=true` 的结构化日志记录谁对哪个包做了什么、结果如何。列表里的状态：服务端部分不运行时是 `disabled`、`installed`（没有 `resident` 服务端部分）、`idle`（`on_demand`）、`broken`（不再通过校验）或 `revoked`；运行时是监管的进程状态。字段见 [API 参考](../reference/api.md)。

### 本地管理员兜底

`users.local_admin` 标记谁在所有插件掉线、被卸载或崩溃循环时仍能恢复站点（ADR 0071 决策 11）：

- 迁移 225 给升级前已有的全部管理员设上；首次启动创建的管理员带上；离线的 `gpuworks reset-password` 给管理员重新设上（普通用户不受影响）。之后新建的用户默认没有，控制台里改自己的密码不改动它。
- 生效值在每个请求读用户时计算：`local_admin && role == admin && 登录会话`。被降级的用户立即失去兜底，不需要写库；平台 API Token 永远不带；它不签进 JWT，也从不从 JWT 读取。`GET /api/v1/auth/me` 的 `local_admin` 返回这个生效值，只用于显示。
- 恢复面是核心路由：登录、`/auth/me`、修改密码与 `/api/v1/plugins/*` 不经过功能门，也不向插件提供的来源（功能可用性、横幅）发问。契约测试 `TestContractLocalAdminDuringCrashloop` 让两个插件都进入崩溃循环（一个保持全部节点，一个把集群卡数上限推成 0），本地管理员仍能重新登录、列出、停用并卸载每个包，每次调用在 10 秒内返回。

推送的约束、准入保持与推送角色的执行器还不存在，所以目前没有地方需要按这个标记豁免；它们出现时先查这个标记。

### 尚未实现

- 插件路由在 `/api/v1/x/<id>/` 下的动态挂载、`on_demand` 插件的按需启动、宿主接口里状态与健康之外的端点。
- 覆盖的管理路由（`State.Override` 已实现）、卸载前的影响预览（`DELETE` 先回 409 列出影响）、升级时的批准差异与自动回滚、`restart`、`logs`、`state` 与目录路由、`gpuworks plugin` 的 `upgrade`、`rollback`、`clock reset` 子命令。
- 站点的宽限上限设置（`pushstate.Config.MaxDownGrace` 已实现，`server.yaml` 还没有对应的键）、没有 cgroup 时的 RSS 看门狗、HTTP/2 窗口耗尽的卡死检测、高可用下只在 leader 上运行插件。
- 核心代码功能键字面量的棘轮。
- 清单词汇里除 `table.<种类>` 以外的能力都还不提供：读取面（`host.*`）、`host.derive_key`、`host.notify`、`metrics.expose`、设置（`host.config`）、`routes` 与 WebSocket 路由（`routes.websocket`），以及部件能力 `node.app`（`node:workloads`、`node:jobs` 也依赖它）、`ui.bundle` 与 `host.setup`。`requires` 它们的包以 `PLUGIN_INCOMPATIBLE` 拒绝，只在 `uses` 里的照常安装但得不到服务（[权限](../reference/plugin-protocol.md#权限)）。

### 跳板：`gpuworks plugin-exec`

server 用自己的二进制拉起每个插件：`gpuworks plugin-exec --profile <json> -- <可执行文件> serve`（`internal/server/pluginexec`，不是给人用的子命令）。跳板以将来的插件进程身份施加沙箱，然后 `execve` 插件，所以沙箱由宿主施加，插件无法撤销：

| 步骤 | Linux | macOS |
|---|---|---|
| 父进程死亡信号 | `PR_SET_PDEATHSIG(SIGKILL)`，并核对父进程仍是 server，否则不启动 | 无；SDK 靠 fd EOF 与轮询 `getppid` 退出 |
| 权限 | `PR_SET_NO_NEW_PRIVS` | — |
| 进程组 | 独立进程组，server 按组发信号 | 同左 |
| 资源 | `RLIMIT_CORE=0`、`RLIMIT_NOFILE=4096`、nice 5；`RLIMIT_NPROC` 按用户计数，插件与 server 同一用户时不设，进程数由 cgroup 的 `pids.max` 限制 | 同左（无 cgroup） |
| 描述符 | fd 3、4 必须是 socket；stdin、stdout 指向 `/dev/null`；stderr 是日志 | 同左 |
| 环境变量 | 只保留 `PATH`、`TZ`、代理与 CA 变量，另设 `HOME`=数据目录、`TMPDIR`、`LANG=C.UTF-8` 与 `GWX_*`；代理变量原样传入，代理 URL 里的用户名密码因此也交给每个插件，代理凭据应只授予插件也可以用的权限 | 同左 |
| cgroup | server 的 cgroup 已委派（`subtree_control` 可写）时建 `<服务 cgroup>/plugins/<id>`，写 `memory.max`、`memory.high`、`cpu.weight=50`、`pids.max=512` 并迁入；做不到只告警，插件照常启动 | — |
| Landlock | ABI ≥ 1：包目录可读可执行，数据与 tmp 目录全部权限，CA、`resolv.conf`、`hosts`、时区只读，批准的设备节点读写；ABI ≥ 6 另禁止向域外进程发信号（server 与其他插件） | 无 |

要求 Landlock 的包（证书 `min_isolation: landlock`）在没有 Landlock 的主机上不启动：跳板以退出码 78 退出并在 stderr 写出 `PLUGIN_ISOLATION_UNAVAILABLE`。跳板自己的其他退出码：64 命令行有误，71 施加沙箱失败，127 无法执行插件。

### 监管

`internal/server/pluginhost` 为每个包运行一个监管者。拉起插件的 goroutine 在插件存活期间一直锁定在自己的 OS 线程上：Linux 的父进程死亡信号属于 fork 子进程的那个线程，而 Go 运行时会回收锁定线程的 goroutine 结束时的线程，不锁定的话别的 goroutine 结束时就可能把插件一起杀掉。

| 项 | 默认值 |
|---|---|
| `initialize` 应答期限 | 10 秒 |
| 健康探测 | 每 10 秒一次，单次 3 秒；连续 3 次未通过则杀掉重启（`PLUGIN_UNHEALTHY`） |
| 崩溃重启 | 1 秒起翻倍，最长 5 分钟；连续健康运行 10 分钟后重新从 1 秒算 |
| 崩溃循环 | 10 分钟内崩溃 5 次进入 `crashloop`（`PLUGIN_CRASHLOOP`），之后每 5 分钟重试一次，无人值守的离线站点能自己恢复 |
| 停止 | `POST /gwx/v1/shutdown`，10 秒后向进程组发 SIGTERM，再 5 秒 SIGKILL |
| 调用并发 | 每个插件同时至多 `max_concurrency`（默认 8）个调用，满了立即回 `PLUGIN_BUSY`，不排队；健康探测不占名额 |
| 无截止时间的调用 | 60 秒 |
| stderr | 转发到 server 日志并带 `plugin=<id>`，JSON 行保留级别；每秒 50 行、突发 500 行，超出的计数后告警；最近 1 MiB 保留供诊断 |

`resident` 插件在加入后立即启动；`on_demand` 插件在第一次调用时启动，空闲 10 分钟后停止，且不能推送状态（`PLUGIN_STATE_ON_DEMAND`）；安装与重新校验时，持有 `push:` 权限或声明功能的 `on_demand` 包同样以这个码拒绝。状态只有 `running` 与 `degraded` 算在线，其余（`starting`、`backoff`、`crashloop`、`stopping`、`stopped`、`idle`、`failed`）都算掉线。插件以不可重试的理由拒绝握手（如 `PLUGIN_CAPABILITY_MISSING`）、应答属于别的构建（`PLUGIN_HANDSHAKE_MISMATCH`）或选了未提供、未批准的能力版本（`PLUGIN_CAPABILITY_NOT_OFFERED`）时进入 `failed`，不再重启，直到再次启动。

### gwx/1 连接与宿主接口

每个插件有两对 socketpair。fd 3 上 server 是 HTTP/2 客户端：先 `initialize`（server 的能力、批准的授权、已持有的表、每次拉起新生成的主体密钥），核对应答的 `id`、`version`、`manifest_sha256` 与包一致、`use` 落在提供且批准的范围内，然后探测健康、转发路由与服务调用，每个调用带 `Gwx-Deadline-Ms`。fd 4 上 server 提供宿主接口：`PUT`/`PATCH /host/v1/state/{kind}` 交给推送状态的实现（一个接口），`GET /host/v1/health`；每个插件每秒 20 次、突发 100 次（`PLUGIN_HOST_RATE_LIMITED`）。所有者是这条连接在握手时绑定的包，推送里没有任何身份字段；握手被接受之前到达的推送等待握手结果，握手失败即回 `PLUGIN_HOST_NOT_READY`。

### 插件 SDK：`pkg/pluginsdk`

插件作者用 `pkg/pluginsdk`（与 `pkg/extapi` 一样是独立模块，只依赖标准库与 `pkg/extapi`）：`pluginsdk.Main(cfg)` 打开两个描述符、回答握手（协议与能力协商，拒绝时用 `PLUGIN_PROTOCOL_UNSUPPORTED` 或 `PLUGIN_CAPABILITY_MISSING`）、在握手被接受后运行 `Start`、回答健康、把其他调用交给插件的 `http.Handler`；收到 `shutdown`、server 一端关闭或父进程不在时退出。`PushState` 跳过 server 已持有且摘要相同的表，推送的代数取 `max(自己的计数, 持有的代数)+1`，所以存储被恢复或清空的插件不会因代数过期被拒。`pkg/pluginsdk/plugintest` 在进程内用真实 socketpair 扮演 server，供插件自己的测试使用。

### 推送状态

首批五种推送表（`availability`、`fleet_capacity`、`node_holds`、`banners`、`attestations`）在核心里的部分在 `internal/server/pushstate`，经上文的插件宿主模块接收插件的推送、在启动时读入并注册为钩子：

- **接收**：`State.Push` 接收监管按连接认定的推送方身份（包 id、发布者、证书授予、批准的权限、清单的码、进程代数）和一份推送。`availability` 的每一行只接受该键的权威（`gate: authority` 时为声明它的发布者证书写明的 `authority_id`，`gate: self` 时为声明它的包），无前缀的键只接受项目发布者的包；`fleet_capacity` 要证书授予该特权表，`attestations` 要推送方是自己命名空间的权威，其余要批准的 `push:<种类>`；通知的码必须列在包的清单里。代数不高于已持有或待提交的被拒（`PLUGIN_STATE_STALE`）；被替换的进程的连接也被拒（`PLUGIN_STATE_CONNECTION_REPLACED`）。
- **写入**：待写推送按所有者与种类合并（后到的取代先到的，先到的收到 `superseded_by`），每个 tick（至多 1 Hz）把全部所有者的改动放进一个写事务，再替换一个快照指针；写入失败则什么都不生效（`PLUGIN_STATE_NOT_SAVED`）。持久化用核心迁移建立的 `pstate_*` 带类型表：时间线是步骤行，通知是码列加带类型的参数行，没有 JSON 列；只用于显示的 `display` 不保存。按条目（功能键、节点 id、横幅 id、证明名）写差量。
- **启动**：启动时在任何插件起来之前读入全部推送表；版本标记（`pstate.<种类>`）比本构建新则以 `PLUGIN_STATE_TOO_NEW` 拒绝启动，存储的限制解不开则拒绝启动而不放松，存储的授权解不开则略过（视为缺席）。
- **执行**：功能来源、节点插入守卫、调度保持者（来源 `plugins`）与横幅各是一次原子读加查表，零分配。没有包作答的键为 `locked`，带 `FEATURE_PROVIDER_ABSENT`。`app.NewPushedStateHooks` 把它们适配成 `Host` 的钩子类型。
- **时间**：时间线按 extclock（`max(墙钟, 高水位)`）在各自的时刻生效，插件死了也一样；高水位每分钟和关机时保存。`State.CorrectClock` 一次至多移动 366 天，回调审计并显示管理员横幅；步骤不被消耗，所以往回校正会让之后的步骤重新未生效。
- **掉线**：`State.OwnerDown` 记下持久化的掉线起点（重启不续期），授权在宽限（默认 1 小时，插件可推 60 秒到 24 小时，站点只能调低）加 2 分钟启动延长（每段掉线都加，运行中崩溃的插件也一样，以免区分启动与运行中掉线）之后降为 `min(当前, down_access)`，带 `PLUGIN_UNAVAILABLE`；限制永不失效。已安装但不再通过校验的包保留限制与横幅，只丢弃授权。
- **覆盖与卸载**：`State.Override` 对限制类条目（一个节点保持，或一项容量上限）只能放松，持久化，绑定所有者的代数；所有者再推同一条目时被取代并回调。`State.Uninstall` 在一次提交里删掉所有者的全部行，并请求一次保持的重新应用。
- **容量选择**：`fleet_capacity` 给上限和可选的优先保留节点 `keep`（多个所有者取并集），选哪些节点由核心决定：`keep` 里的节点在前，其余按接入顺序（`internal/server/fleetcap`，满了以后排在后面的节点一律保持），`keep` 一变也会重新选择；它在节点集合主题的变化后合并读一次节点列表（间隔至少 2 秒），插件掉线时加入的节点同样按这条规则放置。还没放置的节点只有计入它会超过上限时才保持；上限一变（包括所有者被卸载），选择都会请求一次保持的重新应用，这样的节点也随之释放。

## 开源代码不做的事

- 不解释任何功能键，不按版本名分支；核心代码不点名任何功能。
- 不在任何请求、插入、状态上报、认领或放置路径上调用进程外的扩展；不在写事务里运行 WASM 之类的扩展代码。
- 不交出 Store 或原始数据库句柄给扩展。
- 不提供 `init()` 注册或新的包级全局变量：一切经 `cli.Options` 与 `app.Config` 传入。
- 不中断正在运行的负载：功能门只作用于新的请求与写入，保持者只停止调度、不驱逐。
