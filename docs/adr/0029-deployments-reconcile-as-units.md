# ADR 0029: 每个部署都按 unit 对账：角色 → unit → rank，一套重试、整组失败与排空

日期：2026-09-29 · 状态：已接受

依据：[部署收敛计划](../../artifacts/archive/2026-09-29-unit-reconcile/plan.md)。

## 背景

普通部署（副本）、普通多机与 PD 部署原来是三条 reconcile 路径，各有一套重试预算、整组重置与排空。
修复已经开始只落在其中一条上：多机启动屏障只加到了 PD，普通多机仍会因各 rank 拉镜像先后不一而错过 rendezvous。

## 决策

**部署表达为若干 unit。** `planning.DeploymentUnits` 是唯一的换算：

| 部署形态 | unit | 每个 unit 的 rank |
|---|---|---|
| 普通，`replicas = N` | N 个，角色为空，彼此可替换 | 1 个；节点是候选列表，取第一个可用的 |
| 普通多机，`nnodes = N` | 1 个 | N 个，按部署节点顺序 |
| PD | 每个角色的每个 unit（ADR 0028） | leader + followers |

**代（generation）是一个 unit 的一次启动。** leader（rank 0）与 followers（headless）共享
`(dist_group, dist_attempt)`，同一轮里一起创建；启动前的屏障确认每个 rank 的节点在线、模型与镜像已就绪
（多机时先把镜像预拉到每台节点）。多机 leader 在调度记录里记下它所属 unit 的 followers
（`planning.ScheduleRecord.Unit`），用来认领 unit。

**匹配只有一处。** `planning.MatchUnits` 从实例行推导哪个实例填哪个 unit：活跃 leader 按角色、节点、钉卡与
所记录的 followers 认领 unit，followers 按代跟随 leader；未被认领的活跃 leader 排空后移除，失去 leader 的
followers 立即移除，失败的 leader 按 unit key 归为该 unit 已用掉的尝试。控制器与网关用同一个推导；
`planning.GroupsIncomplete`（代是否完整）同样被部署状态、网关与控制器共用。

**重试预算是失败的 leader 行。** 行保留作证据，预算按 unit key 计（普通部署各 unit key 相同，即部署级预算）；
尝试号 = 已失败次数 + 1。`deployments.restart_count` 删除（迁移 110）。leader 在调度记录里记下它为哪个 unit
启动（unit key），失败按这个记录归属：不再属于任何 unit 的失败行（unit 被移出放置，或以另一种形状加回）会被
移除，同一 unit 再加回来时预算是满的。预算用完时控制器在最新一次失败上记一条 `UNIT_RETRY_EXHAUSTED`（只记一次）；
运维经 `POST /deployments/{id}/units/retry` 让单个 unit 重新获得预算，不动其他 unit。

**启动前因端口被占而拒绝的尝试不计预算（2026-11-08 修订）。** agent 在拉镜像、建容器之前先检查 server 分配的主机端口，
端口已被别的进程占用时直接上报 `ENGINE_PORT_REFUSED`，这时什么都没有启动。leader 行以这个码失败的尝试立即重试，不等重试间隔；
每个 unit 最早的 10 次这类拒绝不计入预算（`planning.FreePortRefusals`），之后的拒绝与其他失败一样计入，
所以一台一直拒绝的节点最终仍会用尽预算。失败行照常保留，它们占着的端口让下一次分配避开。follower 被拒时 leader 行记的是
`rank N on … failed: …`，leader 的容器可能已经启动，仍按普通失败计。agent 上报拒绝后立即补发一次节点状态，
让下一次分配看到最新的端口占用。引擎启动后 API 端口上答话的是别的模型（`ENGINE_PORT_IN_USE`）不属于这一类：那次尝试已经建了容器。

**代要么完整，要么失败。** 多机集合通信不能替换单个进程：任何 rank 失败、行消失（已停止）或所在节点离线超过
重连宽限，leader 记为失败并移除容器，followers 移除。先推送移除再写失败状态，推送失败时下一轮重试。

**移除等确认。** 正在移除的行在线节点上重推并等 agent 确认停止，新一代在此之前不启动（dGPU 下未确认的 rank
仍占着显存份额）；离线节点无法确认，其行直接删除，容器由重连后的孤儿清理移除。

**形态专属的部分留在形态里。** PD 的网络与镜像漂移检查（跨 unit 配对才需要）、端口段与连接器参数由 PD 形态提供；
一个 rank 的拓扑只有 `planning.RankTopology` 一个构造函数。

## 后果

- 普通多机获得启动屏障；网关按"代是否完整"选 leader，与 PD 一致。
- 普通多机中进入 stopped 的 rank 视为该代失败（此前只在 dGPU 下如此）。
- 升级兼容：此前启动、没有 unit 记录的多机 leader 按"节点一致、rank 数一致"认领，运行中的部署不会被重建；
  已耗尽重置次数的旧部署在升级后按失败的 leader 行重新计预算。
- 形态之间的差异只剩 unit 的产生方式与 PD 专属的启动参数；新增形态只需给出它的 unit。
