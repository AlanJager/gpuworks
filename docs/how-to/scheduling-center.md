# 使用调度中心：定时计划、闲置休眠、唤醒等位与服务等级

本文说明怎样让平台替你执行事先约定好的 sleep / wake 和副本数调整，以及怎样在调度看板上看清每张卡被谁占着、什么在等、接下来要发生什么。

**适用场景**：多个可休眠的模型轮流使用同一批 GPU（例如白天一个模型、夜里另一个），或者模型长时间没人用，希望自动释放显存。

**前置条件**：

- 定时休眠、定时唤醒、闲置休眠要求部署**可休眠**（vLLM，创建时打开），条件见[操作 sleep / wake](sleep-wake.md)。
- 定时副本数只用于单机部署（非 PD、`nnodes` 不大于 1）。
- 服务等级（常驻以外）只用于可休眠的单机部署。
- 调度中心不按负载扩缩容；平台只在服务等级允许时让一个部署为另一个让卡，见[服务等级](#服务等级与按需唤醒)。

## 调度中心做什么

| 功能 | 你事先约定 | 平台到点做 |
|---|---|---|
| 定时计划 | 周几、HH:MM、时区，加上休眠 L1/L2、唤醒或副本数 | 到点执行一次同样的 API 动作 |
| 闲置休眠 | 多久没有用户请求，休眠到 L1 或 L2 | 超时自动休眠 |
| 唤醒等位 | 唤醒放不下时选择“等位” | 卡腾出来后自动唤醒，等满 30 分钟放弃 |
| 让位唤醒 | 唤醒放不下时选择“让占用者休眠后唤醒” | 先休眠占用者，再等位唤醒 |
| 按需唤醒 | 无需设置：平台自己让它睡下的部署（闲置休眠、让卡） | 有请求时网关挂起请求、唤醒部署，醒后放行 |
| 服务等级 | 常驻、在线、分时或离线 | 共用一批卡的部署之间按等级让卡、轮流 |

它们执行的是你手动操作时同一套动作：同一把生命周期锁、同样的资格检查和唤醒准入。自动唤醒和你手动点**唤醒**不会同时通过同一张卡的准入。每个自动动作都写进部署时间线，见[时间线事件](#时间线事件)。

部署详情里的**调度策略**卡片把这些设置收在一处：折叠时用一行摘要显示节点失联重调度的等待时间、定时计划的条数和下一次执行、闲置休眠的设置（部署不可休眠时不显示闲置休眠）；只有管理员能看到**编辑**，点开后每项一个页签，各自保存。节点失联重调度见[节点失联重调度](node-failover.md)。

## 调度看板

进入**调度中心**页面，一页看清：

- **节点与 GPU**：每张卡的总量、醒着的实例占用、休眠实例的残留、可用量；卡上的占用者（部署、醒着或休眠、休眠级别、分配量和残留）；被平台外进程占着的卡会标出。
- **等待放置的部署**：最近 30 分钟内被调度拒绝的部署和原因，原因的写法与部署详情里的放置状态相同。
- **等位中的唤醒**：哪些部署在等、从什么时候开始。
- **未来 24 小时的计划动作**：时间、部署、动作。
- **最近的自动动作**：最近 50 条自动化时间线事件。

节点按页显示，可按名称搜索。接口为：

```bash
curl --fail-with-body -sS \
  "$API_URL/api/v1/scheduling/board?page=1&page_size=20&q=gpu" \
  -H "Authorization: Bearer $TOKEN"
```

响应的关键结构如下（`nodes` 是分页对象，其余四项是全集群范围）：

```json
{
  "nodes": {
    "items": [{
      "uuid": "…", "name": "host2", "online": true,
      "gpus": [{
        "index": 0, "name": "RTX 8000", "memory_bytes": 51539607552,
        "awake_bytes": 0, "sleeping_bytes": 1610612736, "free_bytes": 49928994816, "held_outside": false,
        "occupants": [{
          "deployment_id": 7, "deployment": "qwen", "instance": "…", "state": "asleep",
          "sleep_level": 2, "claim_bytes": 38654705664, "residue_bytes": 1610612736
        }]
      }]
    }],
    "page": 1, "page_size": 20, "total": 3
  },
  "pending": [], "waiting_wakes": [], "upcoming": [], "recent": []
}
```

实际 `free_bytes` 等是字节整数，上面是示意；`residue_bytes` 只在休眠的占用者上出现。

## 定时计划

每个部署可以有若干条计划，每条是“在周几的某个时刻执行一个动作”。在部署详情的**调度策略**卡片里点**编辑**，在**定时计划**页签添加：

1. 选择动作：**休眠**（须选 L1 或 L2）、**唤醒**，或**副本数**（1–32，仅单机部署）。
2. 勾选周几，填写时间（HH:MM）。
3. 时区默认是 server 所在时区，每条可以改成别的 IANA 时区（如 `Asia/Shanghai`）。
4. 保存。可以随时停用、修改或删除一条计划；列表里显示下一次执行时间。

API 在 `/api/v1/deployments/{id}/schedule` 下：

```bash
# 每个工作日 22:00（东八区）休眠到 L2
curl --fail-with-body -sS -X POST \
  "$API_URL/api/v1/deployments/7/schedule" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"action":"sleep","weekdays":[1,2,3,4,5],"time":"22:00","timezone":"Asia/Shanghai","sleep_level":2}'

# 每个工作日 08:30 唤醒
curl --fail-with-body -sS -X POST \
  "$API_URL/api/v1/deployments/7/schedule" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"action":"wake","weekdays":[1,2,3,4,5],"time":"08:30","timezone":"Asia/Shanghai"}'
```

创建返回 201 和这条计划：

```json
{"id":3,"action":"sleep","weekdays":[1,2,3,4,5],"time":"22:00","timezone":"Asia/Shanghai","sleep_level":2,"enabled":true,"next_at":"2026-10-26T14:00:00Z"}
```

| 路由 | 说明 |
|---|---|
| `GET /deployments/{id}/schedule` | 列出，`{"items":[…],"total":n}` |
| `POST /deployments/{id}/schedule` | 新建，201 |
| `PUT /deployments/{id}/schedule/{sid}` | 整条替换，200；请求体与新建相同 |
| `DELETE /deployments/{id}/schedule/{sid}` | 删除，204 |

请求字段：`action`（`sleep` / `wake` / `replicas`）、`weekdays`（0 为周日到 6 为周六，至少一个、不重复）、`time`（`HH:MM`）、`timezone`（空字符串用 server 时区）、`sleep_level`（只用于 `sleep`，必填）、`replicas`（只用于 `replicas`，必填）、`enabled`（默认 true）。

### 到点时怎么执行

- **每个时间点只执行一次**。评估器每 15 秒看一次，某条计划最新的一个时间点晚于它上次处理的时间点就执行。
- **补执行**：server 停机错过的时间点，启动后只补最新的一次，并且这一次不能早于 12 小时；更早的只标记为已处理，不执行。
- **手动操作不会被反复覆盖**：计划只在时间点上动一次，之后你再手动唤醒或休眠，计划不会在下一轮把它改回去。
- **新建或修改的计划从它的下一个时间点起生效**，不追溯过去的时间点。
- 同一个部署有两条计划同时到期（server 停机跨过了两个时间点）时，先执行较早的，较新的最后执行并留下效果。
- 动作失败（部署不可休眠、放不下等）时，时间线记 `SCHEDULE_ACTION_FAILED`，该时间点也算已处理，不会每 15 秒重复尝试。
- **定时唤醒放不下**时，放得下的先醒，其余进入唤醒等位（时间线 `WAKE_WAITING`），不会直接失败。

### 创建时的校验

| 情形 | 响应 |
|---|---|
| 缺字段、周几越界或重复、时间格式不对、`sleep` 没给级别、`replicas` 不在 1–32 | 400 `SCHEDULE_INVALID`，`params.field`、`params.reason` 说明哪里错 |
| 时区 server 不认识 | 400 `SCHEDULE_TIMEZONE_UNKNOWN`，`params.timezone` |
| 对多机或 PD 部署定副本数 | 400 `SCHEDULE_REPLICAS_SHAPE` |
| 部署没打开可休眠 / 引擎不支持休眠（`sleep`、`wake` 动作） | 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` |

多机与 PD 部署的副本逐个列节点，所以不提供定时副本数。

## 闲置休眠

在部署详情的**调度策略**卡片里点**编辑**，在**闲置休眠**页签设置多久没有用户请求就休眠，以及休眠级别。建议 15 分钟、L2；关闭时填 0。

```bash
curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/idle-sleep" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"after_sec":900,"level":2}'
```

成功返回 200 和同样的 `{"after_sec":900,"level":2}`；部署详情里的 `idle_sleep` 字段给出当前设置。

- `after_sec` 为 0（关闭）或 60 到 604800 秒（7 天）；`level` 为 1 或 2，关闭时也要给。越界返回 400 `IDLE_SLEEP_INVALID`（`params.field`），`after_sec` 大于 0 而部署不可休眠返回 400 `SLEEP_NOT_ENABLED` 或 `SLEEP_ENGINE_UNSUPPORTED`。
- 闲置时长从 server 启动、最近一次唤醒、最近一次用户请求三者中最晚的一个算起。所以 server 重启后至少要再观察满一个闲置时长才会休眠，刚唤醒的部署也不会立刻被睡回去。
- 健康检查等平台自发的探测请求不算用户请求，不会重置计时。
- 休眠失败（例如 L1 的主机内存不够）时，时间线记 `SCHEDULE_ACTION_FAILED`，并至少再闲置一个完整时长后才重试。
- 闲置休眠之后收到请求，网关挂起请求并唤醒部署，醒后放行，见[按需唤醒](#按需唤醒)。

## 唤醒等位与让位唤醒

唤醒被 `WAKE_INSUFFICIENT_GPU_MEMORY` 拒绝时，界面给两个选择，对应 `?wait=true` 与 `?yield=true`，用法和返回值见[操作 sleep / wake](sleep-wake.md#放不下时等位与让位)。要点：

- 等位只拿现成的空位，不会让任何人让出；资源被释放后，服务端通常在 15 秒内自动唤醒，等满 30 分钟放弃，时间线记 `WAKE_WAIT_TIMEOUT`。
- 让位先让占用者休眠；占用者不能休眠时整个请求 409 `WAKE_YIELD_BLOCKED`，什么都不改。
- 部署详情在等位期间显示等位横幅和起点（`wake_waiting_since`），可点**取消等位**，对应 `DELETE /deployments/{id}/wake-waiting`。等位起点保存在数据库，server 重启后继续等。
- 卡被平台外的进程占着时，等位和让位都不会生效：唤醒直接 409 `WAKE_GPU_HELD_OUTSIDE`。

## 服务等级与按需唤醒

服务等级决定一个部署与共用同一批卡的其他部署怎么相处，概念与规则见[服务等级](../explanation/service-levels.md)，设计取舍见 [ADR 0059](../adr/0059-service-levels-and-the-residency-arbiter.md)。

| 等级 | 适合 | 行为 |
|---|---|---|
| 常驻（默认） | 核心在线服务 | 从不让卡 |
| 在线 | 用得不连续的实时服务 | 空闲满 5 分钟才让卡；有请求时让分时、离线立即让卡 |
| 分时 | 几个模型轮流用一批卡 | 同级轮流，每轮至少驻留 2 分钟（切换越慢越长）；让给在线 |
| 离线 | 批处理、评测 | 随时让给所有人，进行中的请求被中断；卡空出来后自动恢复 |

在部署详情的**调度策略**卡片里点**编辑**，在**服务等级**页签选择；创建部署时打开**可休眠**后也可以直接选。API：

```bash
curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/service-level" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"level":"offline"}'
```

成功返回 200 和同样的 `{"level":"offline"}`。等级不合法 400 `SERVICE_LEVEL_INVALID`；常驻以外要求可休眠（400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED`）和单机部署（400 `SERVICE_LEVEL_SHAPE`）；与另一个在线部署共用卡时不能设为在线（409 `SERVICE_LEVEL_ONLINE_SHARED`，`params.deployment` 是对方）。部署详情的 `service_level` 字段给出当前等级。

典型用法是实时 + 批处理：实时模型设为在线，批处理模型设为离线，部署在同一批卡上。实时空闲满 5 分钟后让卡，批处理恢复；实时来了请求，批处理立即让卡，请求在网关等实时醒来（L1 切换约 2.5 秒）。

### 按需唤醒

平台自己让部署睡下时（闲置休眠、按服务等级让卡），请求到达时网关不回 503，而是挂起请求、唤醒部署，醒后放行：

- 挂起时长按等级：常驻与在线 2 分钟、分时 10 分钟、离线 30 分钟；超时返回 503，带 `Retry-After`。每个部署最多挂起 256 个请求，超出的立即 503。
- 你或定时计划下的休眠是**保持休眠**（部署详情 `sleep_held` 为 `true`）：请求不唤醒，仍回 503。任何一次唤醒都解除保持。
- 放不下时按服务等级判断占卡者是否让卡；不该让时请求一直等到挂起时长用完。
- 离线部署让卡时进行中的请求被中断：客户端收到 HTTP 200，`finish_reason` 为 `"abort"`，内容被截断，需要自行重试。分时部署让卡前先停止接新请求，最多等 30 秒让进行中的请求结束。
- 被让卡的部署休眠到 L1（可休眠实例启动时已做过一次 L1 预热，钉住的主机内存早已占着；主机放不下时退回 L2）；分时、离线部署在卡空出来后自动恢复。server 重启后，被让卡的部署要等有请求时才唤醒。

## 首次放置节点

单机部署增加副本时，可以指定新副本先在哪些节点启动，见[部署模型](deploy-models.md#部署后调整)中的说明。这与定时副本数配合使用：计划只改数量，放哪台节点由候选池决定。

## 时间线事件

自动动作写进部署时间线（事件类型 `automation`，也出现在看板的**最近的自动动作**里）：

| code | 含义 | 主要参数 |
|---|---|---|
| `AUTO_SLEEP` | 定时计划、闲置休眠或让卡把部署休眠了 | `trigger`（`schedule` / `idle` / `preempt`）、`schedule_id`、`scheduled_at`、`level`、`idle_sec`；让卡时另有 `reason`（`idle` 空闲满时长 / `rank` 对方等级更高 / `turn` 分时轮到对方）、`for_deployment`（为哪个部署让卡）、`abort`（是否中断了进行中的请求）、`instances` |
| `AUTO_WAKE` | 定时计划、唤醒等位、请求或恢复唤醒了实例 | `trigger`（`schedule` / `wake_waiting` / `request` 有请求在等 / `resume` 让卡后拿回）、`schedule_id`、`instances` |
| `AUTO_SCALE` | 定时计划改了副本数 | `trigger`、`schedule_id`、`replicas` |
| `WAKE_WAITING` | 一次唤醒放不下，进入等位 | `trigger`（`schedule` / `operator`）、`instances`、`yielded` |
| `WAKE_WAIT_TIMEOUT` | 等位满 30 分钟放弃 | `trigger`、`instances`、`waited_sec` |
| `SCHEDULE_ACTION_FAILED` | 自动动作没成功，不自动重试 | `trigger`、`action`、`reason`、`reason_code`、`schedule_id`、`level`、`replicas` |
| `YIELDED_FOR_WAKE` | 记在被让位休眠的部署上：为谁让位、休眠到几级 | `deployment`、`instances`、`level` |

节点失联重调度与驱逐迁移的事件（`NODE_REPLACED` 等）见[节点失联重调度](node-failover.md#时间线事件)。

## 限制

- 只有服务等级允许时才让卡：常驻部署从不为别人休眠；让位唤醒仍是运维点名、对方可休眠才会发生。
- 没有按负载的自动扩缩容：副本数只在定时计划到点时变。
- 服务等级只用于单机部署；新部署启动时卡被醒着的低等级部署占着，平台不为启动让卡，部署按放不下等待。
- 定时副本数只用于单机部署。
- 评估器在 server 进程内运行；server 停机期间没有任何自动动作，恢复后只补 12 小时内最新的一次计划。
