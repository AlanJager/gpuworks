# ADR 0066: agent 的状态按所有者与生命周期成组，组合根只做接线

日期：2026-11-15 · 状态：已接受

## 背景

`internal/agent/app` 一个包有 89 个文件、15.2k 行（非测试）；`NodeRuntime` 有 75 个顶层字段、11 把互斥锁、234 个方法，`OnRPC` 一个 switch 有 50 个 case。镜像、模型文件、卡健康、doctor 都挂在这个结构体上、共用它的锁，任一功能的改动都要改它，子系统不能单独测试或替换。

`internal/agent/engine` 把一个实例的状态放在 12 张按 UUID 索引的并行 map 里，每条退出路径各删一部分。描述引擎进程的四张（睡眠级别、预热、L1 主机缓冲、卡上进程）只在容器自己退出时清除，server 正常移除或停止实例时不清。实测（GWKS-321）：同一 UUID 的新容器被上报持有旧进程的 L1 主机缓冲，server 因此对它跳过主机内存检查；新进程也不再预热。

server 侧的 controller 已经按同样的思路整理过：每类记忆一个自带锁的类型（GWKS-304），每个字段由测试要求写明是否按部署清理（GWKS-303）。本 ADR 把它定为 agent 的写法。

## 决策

1. **按所有者与生命周期成组。** 同一个键（实例 UUID、模型文件、镜像引用……）、在同一时刻失效的状态放进一条记录（`struct`），由一张 `map[key]*record` 持有，不再为每个事实各开一张并行 map。一个实例的状态按生命周期分记录：期望的那一代（desired）、一次对账尝试（in-flight、pending、cancel）、一个引擎进程（`processFacts`）。
2. **在唯一的结束位置整条丢弃。** 每种记录只有一处删除：引擎进程的记录在确认容器已删除的地方（`reclaimInstance`）与切换运行时时删除，不在各条退出路径上各删一部分。新的事实加进它所属的记录，就随记录一起清理。
3. **每个字段必须分类。** `TestEveryEngineFieldIsClassified`（与 controller 的 `TestEveryControllerFieldIsClassified` 同一写法）列出 `Engine` 的每个字段：按实例的写明何时丢弃，其余写明是依赖、配置、锁还是全局计数。新增字段在分类之前测试失败。
4. **子系统自带状态与锁，依赖收窄。** 镜像、模型文件、卡健康、doctor 各成一个包或类型，持有自己的字段与锁，依赖只给它需要的接口（driver、reporter、storage guard 等），零值或构造函数即可单独测试。`NodeRuntime` 是组合根：每个子系统一个字段，加上连接与生命周期的接线。
5. **RPC 是表，不是 switch。** `OnRPC` 改为「方法名 → handler」表，handler 由各子系统提供；`NodeRuntime` 只负责查表与公共的错误处理。
6. **棘轮守护。** `scripts/arch-constraints.txt` 的 `agent_node_runtime_fields`、`agent_node_runtime_mutexes`、`agent_onrpc_cases`、`agent_engine_struct_maps` 只降不升（初值 75 / 11 / 50 / 9）；放宽必须写 `# raised:` 说明理由。

## 结果

- 第一步（GWKS-321）：引擎进程的四张 map 收成 `processes map[string]*processFacts`，在 `reclaimInstance` 整条丢弃；`Engine` 的 map 字段 12 → 9；同一 UUID 的新容器不再继承旧进程的事实（`TestANewContainerUnderTheSameUUIDKnowsNothingOfTheOldProcess`，移除与停止两条路径）。
- 后续按子系统逐个抽出（镜像 → 模型文件 → 卡健康与 doctor → RPC 表），每一步单独合并，棘轮随之收紧。

## 代价与不做的事

- `runtimeEpoch` 按 UUID 保留到 agent 进程结束，是有意的：移除时改为一个全局递增的新值，复用的 UUID 永远对不上旧的尝试。代价是每个见过的 UUID 一个 `uint64`。
- 期望那一代与对账尝试的几张 map（`desired`、`inFlight`、`inFlightCancel`、`pending`、`cleanupRequired`）这一步不合并：它们由 reconcile 的串行化协议共同维护，合并要连同那段协议一起改，留给单独的变更。
- 子系统拆分不改变 agent 与 server 之间的协议与行为。
