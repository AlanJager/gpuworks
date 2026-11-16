# 扩展点：单独构建的 server 如何组合

gpuworks 的 server 可以被另一个 Go 模块当作库，加上自己的代码编成一个新的 server 二进制（下文称「单独构建的 server」）。本页说明开源代码为此留下的接口、每个接口在哪个线程上被调用、必须遵守的规则，以及开源代码不做的事。不可逆的取舍见 [ADR 0070](../adr/0070-extension-tiers-and-edition-boundary.md)；术语（扩展模块、功能键、可用性、扩展停止调度）以 `CONTEXT.md` 为准。

扩展相关的文档、示例与测试都按机制本身描述；示例里的发布者、版本名与插件统一用虚构的 `acme`，就像示例域名用 `example.com`。

## 四个层级

一项能力放在能承载它的最低层级：

| 层级 | 运行在哪里 | 适合什么 |
|---|---|---|
| T0 核心 | server、agent | 所有构建都需要的领域原语；社区版本身 |
| T1 编译进来的模块 | server 进程内，Go；控制台部分是构建时叠加的界面模块 | 要在每个管理请求、每次插入或状态事务里做决定，或在任何节点存在之前就要工作的能力 |
| T2 server 插件 | server 的子进程，经 stdio 通信 | 没有界面、不持有自己状态、只对接外部系统的适配器 |
| T3 应用 | 节点上的容器负载 | 有自己的界面、数据和领域流程，只通过平台调度的负载使用 GPU |

本页只描述 T0 留给 T1 的接口。T2 与 T3 的接口尚未实现。

## 组合方式

单独构建的 server 有自己的 `main`，只调用一次 `cli.Main`：

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
| `MountAPI(e)` | 把 `api.Extension` 的路由挂到 `/api/v1/<名>/` |
| `DeclareFeatures(...)` / `ProvideFeatures(src)` | 声明功能键；提供功能来源（全部模块中至多一个） |
| `AddBannerSource(b)` | 控制台每页顶部的横幅 |
| `GuardNodeInsert(g)` | 新节点插入时的守卫 |
| `HoldScheduling(h)` | 调度保持者，按来源名区分，来源重复即拒绝 |
| `ReapplySchedulingHolds()` | 保持者的答案变了：请求对全部节点重新应用一次保持，不等各节点的下次状态上报 |
| `NodeFleet(ctx)` | 全部节点的保持事实（`store.NodeHoldFacts`），按 id（加入顺序）排序，读池上一次查询 |

`Host` 不交出 `*store.Store`，也不交出数据库句柄。`Init` 返回之后再注册会被拒绝（`ReapplySchedulingHolds` 与 `NodeFleet` 不是注册，任何时候都可调用）：有返回值的方法返回 `app.ErrHostClosed`，其他方法 panic。`Init` 出错时 `New` 失败并关闭数据库，同一数据目录可以立即再次启动。

所有 `Init` 结束后，server 先释放本构建没有任何模块认领的扩展停止调度（换了构建或去掉了模块时，它们不会一直留着），有保持者时再对全部节点应用一次保持，然后构建功能门（`featuregate.New`）并交给 API。

## 保持者的重新应用与节点集合

保持者的答案平时在每次状态上报时被问到，所以答案变了的节点最多要等一分钟才被停止或恢复调度。答案变了（例如快照重建后被保持的集合不同了）的模块调用 `ReapplySchedulingHolds()`，server 对全部节点再执行一次 `ApplySchedulingHolds`：

- 不阻塞、不分配内存，可以在任何 goroutine 调用，**唯独不能在钩子里**（`SchedulingHolder.Hold`、插入守卫）：一次重新应用会再问一遍所有保持者，在 `Hold` 里请求就会让全部节点不停地重新应用。
- 请求会合并：一次重新应用等待或执行期间的任意多次请求，最多再引起一次；两次之间至少隔 2 秒，所以答案来回抖动的保持者每 2 秒最多花一次全表写事务，不会变成每个事件一次。
- 重新应用在模块的 `Service` 所在的同一个 `services()` 循环里执行，不占调用方的 goroutine，随 server 停止；失败时记错误日志，从 2 秒起加倍退避（最长一分钟）重试。没有注册保持者时什么也不做。

答案取决于整个节点集合的保持者（例如按加入顺序挑选节点），用下面两样了解集合，而不是在 `Hold` 里记下见过的节点：

- `NodeFleet(ctx)`：全部节点的保持事实，读池上一次查询。只用于启动时和节点集合变化之后合并执行的重算，不在每次上报或每个事件里调用，也不在钩子里调用。先订阅下面的主题再第一次读取，二者之间的变化就不会漏掉。
- 总线主题 `store.NodeFleetTopic`：节点插入（三条插入路径）、删除，以及改变了保持事实（卡数或厂商；id、UUID、加入时间与所属 Kubernetes 集群不会变）的状态上报，各发一个事件，以节点 id 为键；用 `store.NodeFleetChangeOf(ev)` 读出 `{NodeID, Kind}`（`NodeInserted`、`NodeDeleted`、`NodeFactsChanged`），不要解析 `node` 主题的原因文本。没有改变保持事实的例行上报不发事件，所以发布方的开销不变。和所有总线主题一样，跟不上的订阅者只收到每个节点最新的一个事件，所以订阅者把任何事件都当作「这个节点、因而集合变了」，合并后从 `NodeFleet` 重算，不数事件。节点 id 删除后不再复用。

`Service.Run` 由 `App.Run` 的同一个 `services()` 循环启动，与健康检查、卡故障记录、用量报告、通知刷新并列，在控制器启动之后运行；`ctx` 结束时返回，返回的错误只记日志、不重启。同一个 App 可以多次 `Run`（原地重启），所以 `Run` 必须能被再次调用。

## 表与迁移

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

单独构建的 server 的界面模块复制到 `ui/src/extensions/<id>/`（已被 git 忽略），在 `ui/src/extensions/index.ts` 注册，只能 import `ui/src/extensions/sdk.ts` 再导出的组件、hook 与 API 客户端（`ee_ui_internal_imports` 预算为 0）。构建时设置 `GPUWORKS_UI_OUT_DIR` 输出到别处；检出了界面模块却没设这个变量时，`make ui` 与 `make release-packages` 拒绝构建，以免叠加进社区版。界面模块的文案放在自己的 locale 包里合并进 `common`，用 `GPUWORKS_I18N_EXTRA_LOCALES` / `GPUWORKS_I18N_EXTRA_MESSAGES` 跑同一套 i18n 检查。

## 码与文案

扩展产生的失败与拒绝同样遵守 [ADR 0067](../adr/0067-runtime-outcomes-as-codes.md)：UPPER_SNAKE 码、插值参数、英文 fallback。扩展的码与 en/zh 文案放在扩展自己的代码与界面模块里，不进开源的 `messages` 与 locale 文件；没有叠加界面时，控制台显示英文 fallback。

## 升级与安装

- 发布索引在签名内容里写明版本名；索引的版本名与构建的发布通道不符即拒绝，社区版只接受不写版本名或写 `community` 的索引。
- `upgrade` 比较已安装二进制与待安装二进制的 `gpuworks edition`，不同则拒绝，除非带 `--allow-edition-change`；控制台升级遇到版本不同返回 409 `SERVER_EDITION_MISMATCH`。
- `upgrade` 执行的是当前二进制的安装钩子；跨版本升级后按提示用新二进制再执行一次 `install`。
- 从扩展构建回到社区版时，`server.yaml` 里的 `extensions:` 节只产生告警；扩展的表原样保留；扩展停止调度在启动时释放。

## 开源代码不做的事

- 不解释任何功能键，不按版本名分支。
- 不交出 Store 或原始数据库句柄给扩展。
- 不提供 `init()` 注册或新的包级全局变量：一切经 `cli.Options` 与 `app.Config` 传入。
- 不中断正在运行的负载：功能门只作用于新的请求与写入，保持者只停止调度、不驱逐。
