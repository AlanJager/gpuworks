# ADR 0066: agent 的状态按所有者与生命周期成组，组合根只做接线

日期：2026-11-15 · 状态：已接受

## 背景

`internal/agent/app` 一个包有 89 个文件、15.2k 行（非测试）；`NodeRuntime` 有 77 个字段（含匿名 struct 内的）、12 把互斥锁、233 个方法，`OnRPC` 连同它转交的辅助 switch 分发 51 个 RPC。镜像、模型文件、卡健康、doctor 都挂在这个结构体上、共用它的锁，任一功能的改动都要改它，子系统不能单独测试或替换。

`internal/agent/engine` 把一个实例的状态放在 12 张按 UUID 索引的并行 map 里，每条退出路径各删一部分。描述引擎进程的四张（睡眠级别、预热、L1 主机缓冲、卡上进程）只在容器自己退出时清除，server 正常移除或停止实例时不清，容器死掉后被下一次同步直接替换时也不清。实测（GWKS-321）：同一 UUID 的新容器被上报持有旧进程的 L1 主机缓冲，server 因此对它跳过主机内存检查；新进程不再预热；期望睡眠的实例换了新进程后，agent 把旧进程的睡眠当成新进程的，新引擎醒着占满显存，server 却以为它在睡。

server 侧的 controller 已经按同样的思路整理过：每类记忆一个自带锁的类型（GWKS-304），每个字段由测试要求写明是否按部署清理，并由另一个测试真的写入、清理、断言为空（GWKS-303）。本 ADR 把它定为 agent 的写法。

## 决策

1. **按所有者与生命周期成组。** 同一个键（实例 UUID、模型文件、镜像引用……）、在同一时刻失效的状态放进一条记录（`struct`），由一张 `map[key]*record` 持有，不再为每个事实各开一张并行 map。一个实例的状态按生命周期分记录：期望的那一代（desired）、一次对账尝试（in-flight、pending、cancel）、一个引擎进程（`processFacts`）。
2. **记录的边界就是它描述的东西的边界。** 引擎进程的记录在创建容器的地方从空开始，在确认容器已删除的地方（`reclaimInstance`）与切换运行时时整条丢弃；不在各条退出路径上各删一部分。从空开始的那一端兜住了所有不经 `reclaimInstance` 的替换（容器死后被同步直接替换、切换运行时）。新的事实加进它所属的记录，就随记录一起清理。写入记录的代码在该 UUID 的对账尝试之内（inFlight 串行化）；移除等尝试结束后才丢弃，所以丢弃后不会被旧尝试写回。
3. **每个字段必须分类，按实例的清理由测试证明。** `TestEveryEngineFieldIsClassified` 列出 `Engine` 的每个字段：按实例的写明 server 移除实例时是否丢弃，其余写明是依赖、配置、锁还是全局计数；按字符串索引的 map 只能归为按实例。`TestRemovingAnInstanceLeavesNothingUnderItsUUID` 让一个睡眠中的实例在各张表里都留下条目，移除后断言每个标为丢弃的字段都不再持有它。这与 controller 的 `TestEveryControllerFieldIsClassified` 加 `TestForgetDeploymentLeavesNoMemory` 是同一写法。新增字段在分类之前、或标为丢弃却没清理时测试失败。
4. **子系统自带状态与锁，依赖收窄。** 镜像、模型文件、卡健康、doctor 各成一个包或类型，持有自己的字段与锁，依赖只给它需要的接口（driver、reporter、storage guard 等），零值或构造函数即可单独测试。`NodeRuntime` 是组合根：每个子系统一个字段，加上连接与生命周期的接线。
5. **共享状态归组合根，锁不跨子系统。** 几个子系统都要读的配置与共用的服务（网络配置 `netcfg`、模型目录、registry 认证、puller、记录哪些 registry 刚失败过的 `registries`）由组合根持有，以只读的 getter 或快照交给子系统，子系统不持有别人的配置锁。子系统之间不互相取锁：要查另一个子系统的表（peer relay 同时查模型同步与镜像同步），调用对方暴露的窄方法，由对方在自己的锁内回答。任何代码都不同时持有两个子系统的锁。
6. **RPC 是表，不是 switch。** `OnRPC` 改为「方法名 → handler」表，handler 由各子系统提供；`NodeRuntime` 只负责查表与公共的错误处理。
7. **棘轮守护。** `scripts/arch-constraints.txt` 的 `agent_node_runtime_fields`、`agent_node_runtime_mutexes`、`agent_node_runtime_methods`、`agent_onrpc_cases`、`agent_engine_struct_maps` 只降不升（初值 77 / 12 / 233 / 51 / 9），放宽必须写 `# raised:` 说明理由。测量用 `scripts/structcount` 解析语法树：一行多个名字按名字计，指针锁、`sync.Map` 与包内的 map 类型都计，匿名 struct 里的照样计（包一层不算离开），`OnRPC` 转交给辅助 switch 的 RPC 也计，用 `==` 比较的、写成 handler 表或闭包表的键的也计；以该类型为参数的函数（不论挂在谁身上）算作它的方法；只有移进具名子系统类型的才算离开。方法数守护的是行为：字段搬走了，方法还挂在 `NodeRuntime` 上直接碰子系统的状态，不算拆开。
8. **归类规则：入口定所有者，写者定状态。** agent 的业务逻辑从四种入口进来：RPC、状态同步的字段（`proto.DesiredState` / `proto.StateUpdate`）、数据流、连接与进程入口对 `NodeRuntime` 导出方法的调用（状态上报要的事实、其它信封、启动）。每个入口有且只有一个所有者，所有者与各入口的归属只记在 `internal/agent/app/owners_test.go`。新增业务逻辑按下面的顺序归类：
   1. **已有入口**：逻辑写进表里这个入口的所有者。
   2. **新入口**：所有者是这次请求写的那份状态的所有者；只读的，是被读状态的所有者。要写两个所有者的状态时，归串行化这次操作的那把锁的所有者，另一方经它暴露的方法被调用。承载别的入口的入口（peer relay）把载荷原样交给载荷的所有者，自己不解析。定下后在表里加一行；所有者已有自己的 handler 表的，RPC 加进那张表。
   3. **状态归写它的所有者**，连同锁，放在所有者自己的类型里。别的所有者要读它或让它失效，调用它的方法；触发的一方以函数依赖的形式拿到这个方法（`engine.WithLogDrain` 的做法），不取它的锁。
   4. **多个所有者共用的**：同步下来的配置与共用的服务只有决策 5 的名单，由组合根持有，加一项要改本 ADR；不记状态的代码放 `internal/agent/` 下不依赖 `app` 的叶子包。
   5. **后台循环**归它写的状态的所有者；只上报不写的，归被上报状态的所有者。一个循环只服务一个所有者。

   已有自己类型的所有者从严：在 `NodeRuntime` 上只占一个字段，这个字段只在它的访问函数里出现，它的 RPC 全部由自己的表处理。其余所有者的状态暂时逐个字段记在 `fieldOwners` 里。守护的测试：`TestEveryRPCHasOneOwner`、`TestEveryStreamHasAnOwner`、`TestEverySyncedFieldHasAnOwner`、`TestEveryExportedNodeRuntimeMethodHasAnOwner`、`TestEveryNodeRuntimeFieldHasAnOwner`、`TestAnExtractedOwnerIsReachedOnlyThroughItsAccessor`、`TestNoTypeHoldsTheNode`、`TestNoLockIsPassedAround`。

## 结果

- 第一步（GWKS-321）：引擎进程的四张 map 收成 `processes map[string]*processFacts`，创建容器时从空开始，在 `reclaimInstance` 与切换运行时整条丢弃；`Engine` 的 map 字段 12 → 9。同一 UUID 的新容器不再继承旧进程的事实：`TestANewContainerUnderTheSameUUIDKnowsNothingOfTheOldProcess`（server 移除、停止）与 `TestAReplacedEngineProcessIsPutToSleepAgain`（容器死后被同步替换、切换运行时，新进程真的被重新送去睡眠）。
- 第二步（GWKS-503）：镜像成为第一个有自己类型的所有者 `nodeImages`（`internal/agent/app/images.go`），持有目录清单、镜像操作槽位、可取消的操作与对端传输表，连同三把锁；它要的东西由 `imageDeps` 的函数给出。数字见 `scripts/arch-constraints.md`。
- 后续按所有者逐个抽出：模型文件 → 卡健康与 doctor → 其余，每一步单独合并，棘轮随之收紧。全部抽出后 `rpcOwners` 为空，届时删除。

## 代价与不做的事

- `runtimeEpoch` 按 UUID 保留到 agent 进程结束，是有意的：移除时改为一个全局递增的新值，复用的 UUID 永远对不上旧的尝试。代价是每个见过的 UUID 一个 `uint64`。
- 期望那一代与对账尝试的几张 map（`desired`、`inFlight`、`inFlightCancel`、`pending`、`cleanupRequired`）这一步不合并：它们由 reconcile 的串行化协议共同维护，合并要连同那段协议一起改，留给单独的变更。
- 规则 2 的判断（写的是谁的状态）、规则 4 的叶子包、规则 5 的后台循环没有测试，靠评审；测试只保证每个入口与字段都有一行归属，以及已抽出的所有者不被绕过。测试看不出来的：一个所有者的状态被塞进另一个所有者的类型里；把新字段或新导出方法标成 wiring 或 shared（靠评审对照决策 5 的名单）。
- `nodeImages` 留在 `app` 包内、首次使用时才接到 `NodeRuntime` 上，都是过渡：多数测试用 `&NodeRuntime{}` 字面量构造，包内还有几个所有者共用的函数。字面量改用构造函数后改为构造时接线；模型文件抽出、共用函数进叶子包后，两者各自成包。
- 数据流仍由 `conn.Handlers` 上每种一个 `NodeRuntime` 方法转交，没有「kind → handler」表；新增一种数据流要占一个方法的棘轮。
- 子系统拆分不改变 agent 与 server 之间的协议与行为。
