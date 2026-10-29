# Use the Scheduling Center: Schedules, Service Levels and Sleep, and Waiting Wakes

This guide explains how to have the platform carry out sleep, wake, and replica-count changes you agreed on in advance, and how to read the scheduling board to see who holds each card, what is waiting, and what is about to happen.

**Use case**: several sleep-capable models take turns on the same GPUs (for example one model by day and another at night), or a model sits unused for long periods and you want its GPU memory released automatically.

**Prerequisites**:

- Scheduled sleep, scheduled wake, auto sleep, and yield policies require the deployment to be **sleep capable** (vLLM, enabled at creation); see [Sleep and Wake Operations](sleep-wake.md) for the conditions.
- A scheduled replica count applies to single-node deployments and to multi-node and PD deployments placed automatically (`placement_mode` `auto`; a PD deployment is scheduled one role at a time); multi-node and PD deployments placed by hand are not supported.
- Yield policies other than never yield apply only to sleep-capable single-node deployments; a multi-node or PD deployment must use never yield, and auto sleep is not affected.
- The scheduling center does not scale by load. The platform makes one deployment give up its cards for another only when its yield policy allows it; see [Service levels and sleep](#服务等级与休眠).

## What the Scheduling Center Does

| Feature | What you agree in advance | What the platform does when due |
|---|---|---|
| Schedule | Weekdays, HH:MM, and time zone, plus sleep L1/L2, wake, or a replica count | Runs the same API action once at that time |
| Auto sleep when not preempted | Turn on auto sleep, and set the idle time and sleep level | Sleeps once idle for that time, even when no other deployment waits for its cards |
| Waiting wake | Choose “wait” when a wake does not fit | Wakes automatically once cards free up; gives up after 30 minutes |
| Yielding wake | Choose “put the occupants to sleep, then wake” when a wake does not fit | Puts the occupants to sleep first, then wakes through waiting |
| On-demand wake | Nothing to set: deployments the platform put to sleep itself (auto sleep, yielding cards) | When a request arrives, the gateway holds it, wakes the deployment, and lets the request through once it is awake |
| Yield policy and service level | Never yield, yield when idle, time slice, or yield anytime plus parameters, or simply a resident, online, shared, or offline preset | Deployments sharing a set of cards yield cards by policy and take turns |

They carry out the same actions you would take by hand: the same lifecycle lock, the same eligibility checks and wake admission. An automatic wake and your manual **Wake** cannot both pass admission for the same card. Every automatic action is written to the deployment timeline; see [Timeline Events](#时间线事件).

The **Scheduling policy** card in deployment details keeps these settings in one place. Collapsed, it shows one summary line: the wait for node-loss rescheduling, the number of schedule entries and the next run, and a summary of the service level and sleep (not shown when the deployment cannot sleep). Only admins see **Edit**; it opens one tab per setting (service level and sleep share one tab), each saved on its own. For node-loss rescheduling, see [Reschedule Replicas on Node Loss](node-failover.md).

## Scheduling Board

Open the **Scheduling Center** page. The upper half shows the whole cluster, the lower half the queues and the history:

- **Summary strip**: the total number of cards in the cluster; how many are serving, asleep, free, held by a process outside the platform, faulty, or on an offline node (six disjoint classes that add up to the total); the cards and free cards by GPU model, for example "L20 ×320, 41 free"; and contention - wakes waiting for room, requests the gateway holds for a wake right now, yields in the last hour - plus the deployments waiting for placement and the schedule actions in the next 24 hours. These figures are computed by the server for the whole cluster and do not change with the filters below.
- **GPU heatmap**: one small cell (about 12 px) per card, grouped by node; 1000 nodes of 8 cards fit on one or two screens. The colour is the card's state, with the legend at the top right of the card:

  | Colour | Meaning |
  |---|---|
  | Blue, deeper = more claimed | Serving: an instance holds memory on the card; the depth is the share of memory claimed |
  | Purple | Asleep: only the residue of sleepers is on the card |
  | Light green with a green border | Free |
  | Orange | Held by a process outside the platform |
  | Red | Faulty, or unusable because of its ownership or health |
  | Grey, hatched | Its node is offline (as last reported, so possibly stale) |
  | Black inner outline | An instance on the card is starting, going to sleep, waking or draining |

  Hover or keyboard-focus a cell to see the card's details: node, card index, model, memory, the shares that are serving, asleep and free, and its occupants (deployment, service-level preset, serving or asleep). The heatmap itself carries no occupant detail, so even a thousand-card fleet costs one read of about 290 KB (about 25 KB gzipped).
- **Filters and view**: filter by GPU model (several allowed), node label (`key=value`) and node state (has free cards, has sleepers, has cards held outside, offline); type a part of a deployment name in "Find a deployment" and the cards it occupies get a red ring while the rest fade, with a count of the occupied cards. Group by node (the default), by GPU model, or by the value of a node label; sort by name, by free capacity (the unclaimed memory of the cards, not counting offline, faulty or externally held cards) or by contention (sleepers waiting to wake, plus the requests held for deployments asleep on the node, plus the yields of deployments that have cards on it). **Clear filters and view** returns to the default.
- **Node drawer**: click a cell or a node name and a drawer opens that node: a bar per card split into serving usage, the residue memory of sleepers and free memory, the occupants of each card (deployment and its service-level preset - resident, online, shared, offline or custom - serving or asleep, sleep level, claim and residue), cards held by processes outside the platform marked, and below them the node's [residency timeline](#residency-timeline). The drawer reads only that one node, and only when it is open.
- **Deployments waiting for placement**: deployments the scheduler refused in the last 30 minutes, with reasons written the same way as the placement status in deployment details.
- **Waiting wakes**: which deployments are waiting and since when; the wait can be cancelled.
- **Schedule actions in the next 24 hours**: time, deployment, action.
- **Deployments sharing cards**: the stats, over a 6 or 24 hour window, of the deployments across the whole cluster whose yield policy is not never yield; see [Residency Timeline](#residency-timeline).
- **Scheduling history**: when, why and what was scheduled; see [Scheduling History](#调度记录).

The heatmap works from the keyboard alone: Tab enters it and the focus rests on one cell; Left and Right move between cards (continuing across nodes), Up and Down move between neighbouring nodes at the same position, Page Up and Page Down jump ten nodes, Home and End go to the first and last card; Enter or Space opens that node's drawer. The page refreshes every 10 seconds and redraws nothing when the cluster has not changed.

The server keeps the state of the whole cluster in a memory cache. Changes to nodes, instances and deployments invalidate it, but it is rebuilt at most every 2 seconds, so the status reports of a large fleet do not make every refresh recompute it. The API is:

```bash
curl --fail-with-body -sS "$API_URL/api/v1/scheduling/overview" -H "Authorization: Bearer $TOKEN"

curl --fail-with-body -sS "$API_URL/api/v1/scheduling/board?node=$NODE_UUID" -H "Authorization: Bearer $TOKEN"
```

The overview's structure is in the [API reference](../reference/api.md#调度总览). `GET /api/v1/scheduling/board` can still be read page by page (`page`, `page_size`, `q`); the main structure of its response is below (`nodes` is a paged object; the other four items cover the whole cluster):

```json
{
  "nodes": {
    "items": [{
      "uuid": "…", "name": "host2", "online": true,
      "gpus": [{
        "index": 0, "name": "RTX 8000", "memory_bytes": 51539607552,
        "awake_bytes": 0, "sleeping_bytes": 1610612736, "free_bytes": 49928994816, "held_outside": false,
        "occupants": [{
          "deployment_id": 7, "deployment": "qwen", "preset": "shared", "instance": "…", "state": "asleep",
          "sleep_level": 2, "claim_bytes": 38654705664, "residue_bytes": 1610612736
        }]
      }]
    }],
    "page": 1, "page_size": 20, "total": 3
  },
  "pending": [], "waiting_wakes": [], "upcoming": [], "recent": []
}
```

The real `free_bytes` and the other sizes are integer bytes, and the values above are only illustrative; `residue_bytes` appears only on sleeping occupants. `recent` (the latest 50 automation events) is kept for compatibility; the page reads the scheduling history below instead.

### Residency Timeline

The **Residency timeline** in the node drawer draws a horizontal time axis for every card of that node: one lane for each deployment that held the card, with blocks coloured by the state it was in during that stretch:

| State | Meaning |
|---|---|
| Serving | The instance was running and could serve requests |
| Asleep | The engine slept and only its residue stayed in memory; L1 or L2 is shown |
| Switching | Between a sleep or wake order and its result |
| Starting | Being created, pulling its image or loading; not serving yet |

The time ticks are drawn to scale. Hover a block, or focus it with the keyboard, to see the deployment, the state and the start and end times. The window is 6 or 24 hours.

The **Deployments sharing cards** table on the page lists the deployments across the whole cluster that held cards in the window and whose yield policy is not never yield (the drawer shows the same table under its timeline, limited to the deployments that held that node's cards):

| Column | Meaning |
|---|---|
| Serving time, share | The time at least one replica was serving, and its share of the window |
| Switches | Times it yielded plus times the platform woke it |
| Yielded, yielded to | Times it was put to sleep to give a card to another deployment, and times another deployment gave a card to it |
| Held requests, wait p50 / p95 | Requests the gateway held while it woke, and the median and 95th percentile of their waits |

The endpoint is separate so that it does not slow down the overview, which refreshes every few seconds:

```bash
curl --fail-with-body -sS \
  "$API_URL/api/v1/scheduling/timeline?node=$NODE_UUID&hours=6" \
  -H "Authorization: Bearer $TOKEN"

curl --fail-with-body -sS \
  "$API_URL/api/v1/scheduling/timeline?sharing=1&hours=6" \
  -H "Authorization: Bearer $TOKEN"
```

What the figures mean and where they stop:

- The timeline is derived from instance records and the event log; nothing else is stored for it. Removing an instance now also records an `INSTANCE_REMOVED` event naming the node and cards it held; instances removed before the upgrade have no such record and do not appear in past segments.
- With `node`, only the nodes named in the request count: replicas of a deployment on other nodes are not part of its serving time, and "yielded to" counts only the deployments that also held these cards giving way to it. `sharing=1` counts all the instances of these deployments across the cluster, reads only as much as the number of sharing deployments, and its result is cached for about 30 seconds.
- A follower of a multi-node replica follows its leader's sleep and wake.
- At most the latest 200 segments of a card are returned; older ones are left out and the console says so.
- The wait quantiles approximate the window: the gateway's quantile of each flush interval, weighted by the requests of that interval, not an exact per-request quantile of the window. The counts start again from zero when the gateway restarts, and both columns stay empty when no request was held in the window. The metric the gateway records is `wake_hold_wait_ms`, see [Inference Metrics](../reference/inference-metrics.md).

## Schedules

Each deployment can have any number of entries, each of which is “do an action at a time on certain weekdays”. In the **Scheduling policy** card in deployment details, click **Edit** and add them on the **Schedule** tab:

1. Choose the action: **Sleep** (pick L1 or L2), **Wake**, or **Replica count** (1–32, single-node deployments and automatically placed multi-node and PD ones only; a PD deployment also asks for a **role**, see below).
2. Tick the weekdays and enter the time (HH:MM).
3. The time zone defaults to the server's own; each entry can use a different IANA time zone (such as `Asia/Shanghai`).
4. Save. You can disable, edit, or delete an entry at any time; the list shows the next run time.

The API lives under `/api/v1/deployments/{id}/schedule`:

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

Creating returns 201 and the entry:

```json
{"id":3,"action":"sleep","weekdays":[1,2,3,4,5],"time":"22:00","timezone":"Asia/Shanghai","sleep_level":2,"enabled":true,"next_at":"2026-10-26T14:00:00Z"}
```

| Route | Description |
|---|---|
| `GET /deployments/{id}/schedule` | List, `{"items":[…],"total":n}` |
| `POST /deployments/{id}/schedule` | Create, 201 |
| `PUT /deployments/{id}/schedule/{sid}` | Replace an entry, 200; the body is the same as for creating |
| `DELETE /deployments/{id}/schedule/{sid}` | Delete, 204 |

Request fields: `action` (`sleep` / `wake` / `replicas`), `weekdays` (0 for Sunday through 6 for Saturday, at least one, no repeats), `time` (`HH:MM`), `timezone` (an empty string uses the server's time zone), `sleep_level` (only for `sleep`, required), `replicas` (only for `replicas`, required), `role` (only for `replicas`: required for a PD deployment, refused for any other), and `enabled` (default true).

### What Happens When an Occurrence Is Due

- **Each occurrence runs once.** The evaluator looks every 10 seconds and runs an entry when its newest occurrence is later than the one it last handled.
- **Catch-up**: for occurrences missed while the server was down, only the newest one runs after it starts, and that one may not be older than 12 hours; older ones are only marked handled, not run.
- **Manual operations are not overridden repeatedly**: an entry acts once at its time, and if you then wake or sleep by hand, the entry does not change it back on the next round.
- **A new or edited entry takes effect from its next occurrence** and does not look back at earlier ones.
- When two entries of one deployment are due together (the server was down across both), the earlier runs first and the newer runs last and stands.
- When an action fails (the deployment cannot sleep, it does not fit, and so on), the timeline records `SCHEDULE_ACTION_FAILED` and the occurrence still counts as handled; it is not retried on every tick.
- **A scheduled wake that does not fit**: replicas that fit wake first and the rest go into a waiting wake (timeline `WAKE_WAITING`) rather than failing outright.

### Validation When Creating

| Case | Response |
|---|---|
| Missing fields, weekdays out of range or repeated, a bad time format, `sleep` without a level, `replicas` out of range (1–32; 0–32 for a PD role the mode lets be empty) | 400 `SCHEDULE_INVALID`; `params.field` and `params.reason` say what is wrong |
| A `replicas` entry of a PD deployment with no `role` or a role its mode does not have; a `role` on a deployment that is not PD; a `role` on a `sleep` or `wake` entry | 400 `SCHEDULE_INVALID` with `params.field` `role` (for a PD deployment `params.roles` lists the roles to choose from) |
| The server does not know the time zone | 400 `SCHEDULE_TIMEZONE_UNKNOWN`, `params.timezone` |
| A replica count for a multi-node or PD deployment placed by hand | 400 `SCHEDULE_REPLICAS_MANUAL_PLACEMENT` |
| The deployment is not sleep capable or the engine cannot sleep (`sleep` and `wake` actions) | 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` |

**A scheduled replica count for a multi-node deployment** takes the same path as the "grow or shrink by count" of `PUT /api/v1/deployments/{id}/replicas`: when growing, the server picks nodes for the new replicas by the placement strategy (the rules of [how nodes are picked at creation](multi-node-inference.md#创建时怎样挑节点)); when shrinking, the replicas added last go. If nothing fits, the `reason_code` of `SCHEDULE_ACTION_FAILED` is `UNIT_PLACEMENT_NONE`. A multi-node deployment placed by hand lists the nodes of every replica, so someone has to name the nodes of an added replica, and it is not supported.

**A scheduled replica count for a PD deployment is set per role**: the entry's `role` is a role of its mode (such as `prefill` or `decode`) and `replicas` is the number of instances of that role; the other role is left alone. When due it takes the same path as `PUT /api/v1/deployments/{id}/pd`: when growing, the server picks nodes for the new units beside the placed ones (as when an automatically placed PD deployment is created); when shrinking, the units added last go. If nothing fits, the `reason_code` of `SCHEDULE_ACTION_FAILED` is `UNIT_PLACEMENT_NONE`; any other failure (say, the role's units span several nodes while a new unit gets one) keeps the PD update's own error code and `reason`. A role's minimum holds as well: a role the mode lets be empty (`prefill` in `vllm-nixl`) may be scheduled to 0, every other role needs at least 1, so a number below the minimum is refused when the entry is created; `decode` at 0 returns 400. A deployment placed by hand is refused likewise (`SCHEDULE_REPLICAS_MANUAL_PLACEMENT`).

If a deployment is switched to manual placement after its schedule was written, the entry fails when it is due: the timeline records `SCHEDULE_ACTION_FAILED` (`reason_code` `SCHEDULE_REPLICAS_MANUAL_PLACEMENT`) and the entry itself stays, so it runs as usual at the next occurrence after the deployment goes back to automatic placement.

## Waiting Wakes and Yielding Wakes

When a wake is refused with `WAKE_INSUFFICIENT_GPU_MEMORY`, the console offers two choices, corresponding to `?wait=true` and `?yield=true`; for usage and responses see [Sleep and Wake Operations](sleep-wake.md#放不下时等位与让位). In brief:

- Waiting takes only room that is already there and makes no one give way; once capacity is released the server usually wakes the deployment within 10 seconds, and gives up after 30 minutes with `WAKE_WAIT_TIMEOUT` in the timeline.
- Yielding puts the occupants to sleep first; if an occupant cannot sleep, the whole request is refused with 409 `WAKE_YIELD_BLOCKED` and nothing changes.
- While waiting, the deployment details show a waiting banner and the start (`wake_waiting_since`), and you can click **Cancel waiting**, which maps to `DELETE /deployments/{id}/wake-waiting`. The start of the wait is stored in the database, so the wait continues after a server restart.
- When a process outside the platform holds the card, neither waiting nor yielding takes effect: the wake is refused outright with 409 `WAKE_GPU_HELD_OUTSIDE`.

## Service Levels and Sleep

How a deployment gets along with the other deployments that share its cards, and whether it puts itself to sleep when nobody uses it, is decided by one **yield policy** plus a few parameters that appear only when they apply. For the concepts and rules see [Yield policy and service levels](../explanation/service-levels.md), and for the design trade-offs see [ADR 0059](../adr/0059-service-levels-and-the-residency-arbiter.md). The four **service levels** (resident, online, shared, offline) are presets of it: the deployment stores only the policy and parameters, the preset is derived by comparing them with the four presets, and changing any item shows it as **custom**.

### Yield Policy and Parameters

The yield policy decides only who may preempt whom:

| Yield policy | Meaning | Rank |
|---|---|---|
| Never yield (`never`) | Never gives its cards to another deployment; once put to sleep by the platform, it can take cards from the three below when it has requests | Highest |
| Yield when idle (`idle`) | Yields only when another deployment waits for its cards and it has been idle for the idle time | Can preempt time slice and anytime |
| Time slice (`timeslice`) | Takes turns with the other time-slice deployments on the same cards (each turn lasts at least the minimum serving time, and the waiter's oldest request must have waited 30 seconds); yields at once to never-yield and yield-when-idle waiters; yields to any waiter once idle for the idle time | Can preempt anytime |
| Yield anytime (`anytime`) | Yields at once to higher-ranked waiters; to a waiter of the same policy only after it has been idle for a fixed 30 seconds (not configurable) | Lowest |

Two yield-when-idle deployments cannot share a card. The UI shows a parameter only when it applies; the API accepts inapplicable parameters, ignores them, and leaves them out of responses:

| Parameter | Values | Applies to |
|---|---|---|
| `idle_sec` idle time | 30–604800 s | Yield when idle and time slice; any policy when auto sleep is on. The UI shows “—” under never yield and yield anytime when auto sleep is off |
| `auto_sleep` auto sleep when not preempted | On / off, default off | Every sleep-capable deployment, including multi-node and PD |
| `sleep_level` sleep level | `auto` (L1, falling back to L2 when host memory cannot hold the weights), `l1` (strict L1, refused where it does not fit), `l2` | A policy other than never yield, or auto sleep on |
| `min_serve_sec` minimum serving time | 0–3600 s | Time slice |
| `unfinished` handling of unfinished requests when yielding | `abort` cuts them off at once; `wait` lets them finish, for at most `unfinished_wait_sec` (1–600 s) and then cuts off what is left; `wait_unbounded` lets them finish with no time limit | Time slice and yield anytime |
| `hold_max_sec` longest request hold | 10–1800 s | A policy other than never yield, or auto sleep on |

- **Auto sleep when not preempted**: when on, the deployment also sleeps once idle for `idle_sec`, even if no other deployment waits for its cards. The idle time is counted from the latest of server start, the last wake, and the last user request, so after a server restart the platform has to observe a full idle period before it sleeps, and a deployment that has just woken is not put straight back to sleep. Platform-originated probes such as health checks are not user requests and do not reset the timer. When the sleep fails (for example `l1` and not enough host memory), the timeline records `SCHEDULE_ACTION_FAILED`, and the platform retries only after at least another full idle period. It is a sleep the platform chose, not a held sleep: a later request makes the gateway hold it and wake the deployment; see [On-demand wake](#按需唤醒).
- **Sleep level** applies to every sleep the platform chooses for the deployment (a yield and an auto sleep); sleeps an operator orders by hand or a schedule entry specifies use their own level and are unaffected.
- **`wait` and `wait_unbounded` of `unfinished`**: when yielding, the gateway first stops accepting new requests for this deployment (new requests are held), and puts it to sleep once the in-flight requests finish. `wait` waits at most `unfinished_wait_sec` seconds and then cuts off the requests that are left; `wait_unbounded` waits indefinitely, so one very long request can keep the waiter waiting forever, and the waiter's requests are held at the gateway for at most its own `hold_max_sec` and then get a 503. When unsure, use the bounded `wait`.
- **`min_serve_sec`** is a lower bound: the residency arbiter raises it to 10 times the measured switch time when that is larger.

### Configuring It

In the **Scheduling policy** card in deployment details, click **Edit** and use the **Service levels and sleep** tab:

1. The row of radio buttons at the top is the preset: **Resident**, **Online**, **Shared**, **Offline**, **Custom**. Choosing a preset fills in its whole set of parameters below; once any parameter differs from the preset, the selection jumps to **Custom** by itself.
2. The **Yield policy** select chooses one of the four policies.
3. Below it only the parameters that apply to the current policy are shown; the **Auto sleep when not preempted** switch is there for every policy.
4. Save. Collapsed, the card's summary reads “Service level: Online”, or, when custom, “Service level: Custom · yield when idle (idle 30 s, L1)”.

When creating a deployment (with **Sleep capable** on) there is only a preset select (resident, online, shared, offline); custom is edited after creation.

The API has a single entry point, `PUT /api/v1/deployments/{id}/yield-policy`. The first request below sets the online preset and the second a custom policy (yield when idle, idle for 30 s, auto sleep when not preempted, strict L1). The body is one of two forms: `{"preset":"…"}` with the preset alone, or `policy` plus any parameters, where a missing parameter takes the policy's default (the values of that policy's preset; `auto_sleep` defaults to off; `idle_sec` defaults to 300 when it applies only because of auto sleep; `hold_max_sec` defaults to 120 for never yield). When `policy` is given, `preset` is ignored, so a response can be sent back as is.

```bash
curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/yield-policy" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"preset":"online"}'

curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/yield-policy" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"policy":"idle","auto_sleep":true,"idle_sec":30,"sleep_level":"l1","hold_max_sec":120}'
```

Success returns 200 with the normalized policy and the derived preset, with inapplicable parameters absent. The first request returns:

```json
{"preset":"online","policy":"idle","auto_sleep":false,"idle_sec":300,"sleep_level":"auto","hold_max_sec":120}
```

The second returns `"preset":"custom"`. The `yield_policy` field in deployment details gives the current setting (same shape).

| Situation | Response |
|---|---|
| The body is `{}`, or has parameters but no `policy` | 400 `YIELD_POLICY_INVALID`, with `params` of `{"field":"policy","reason":"required"}` |
| A value is not one of the allowed ones, or a number is out of range | 400 `YIELD_POLICY_INVALID`; `params.field` is the field and `params.reason` is `invalid_value` or `out_of_range` (which also carries `minimum` and `maximum`) |
| The deployment is not sleep capable / the engine does not support sleep, and the policy is not never yield or auto sleep is on | 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` |
| A multi-node or PD deployment with a policy other than never yield | 400 `YIELD_POLICY_SHAPE` |
| Setting yield when idle while another yield-when-idle deployment is on the same cards | 409 `YIELD_POLICY_IDLE_SHARED`; `params.deployment` is the other one |

### Service Levels and On-Demand Wake

The four service levels are presets:

| Preset | Good for | Policy and parameters |
|---|---|---|
| Resident (default) | Core online services | Never yield, auto sleep off |
| Online | Real-time services used intermittently | Yield when idle, idle 300 s, sleep level `auto`, longest request hold 120 s, auto sleep off |
| Shared | Several models taking turns on a set of cards | Time slice, idle 60 s, minimum serving time 120 s (longer when switching is slower), unfinished requests wait to finish (at most 30 s), sleep level `auto`, longest request hold 600 s, auto sleep off |
| Offline | Batch work, evaluation | Yield anytime, unfinished requests aborted at once, sleep level `auto`, longest request hold 1800 s, auto sleep off; resumes automatically once cards are free |

A typical use is real-time plus batch: set the real-time model to online and the batch model to offline, and deploy them on the same cards. After the real-time model has been idle for 5 minutes it yields and batch work resumes; when a real-time request arrives, the batch model yields at once and the request waits at the gateway for the real-time model to wake (an L1 switch takes about 2.5 seconds). For how a batch job points at an offline deployment, see [Run Batch Jobs](batch-api.md#与离线服务等级搭配).

#### On-Demand Wake

When the platform itself put a deployment to sleep (auto sleep, or yielding cards), a request that arrives does not get a 503 from the gateway. The gateway holds the request, wakes the deployment, and lets the request through once it is awake:

- The hold time is the deployment's `hold_max_sec` (2 minutes when it has none); on timeout it returns 503 with `Retry-After`. Each deployment holds at most 256 requests, and beyond that returns 503 at once.
- A sleep ordered by you or by a schedule is a **held sleep** (`sleep_held` is `true` in deployment details): a request does not wake it, and the gateway still answers 503. Any wake clears the hold.
- When it does not fit, the yield policy decides whether the occupant yields; when it should not, the request waits until the hold time runs out.
- When a time-slice or yield-anytime deployment yields its cards, its in-flight requests are handled by `unfinished`: `abort` cuts them off at once; `wait` and `wait_unbounded` first stop accepting new requests (new requests are held) and wait for the in-flight requests to finish, and `wait` cuts off whatever is still running at `unfinished_wait_sec`. An interrupted **non-streaming** request is held by the gateway and replayed once the deployment is back (up to 3 times), so the client sees only the finished result; a **streaming** request has already started to produce output and cannot be replayed: the client sees HTTP 200, a `finish_reason` of `"abort"` and truncated content, and must retry. The platform-managed [batch jobs](batch-api.md#与离线服务等级搭配) retry interrupted requests automatically.
- A deployment that yields its cards sleeps at its `sleep_level` (with `auto`, L1 first; every sleep-capable instance already ran an L1 warm-up at start, so the pinned host memory is already held; it falls back to L2 when the host cannot fit it); time-slice and yield-anytime deployments resume automatically once cards are free. After a server restart, a deployment that yielded its cards is woken only when a request arrives.

## 调度记录

The **Scheduling history** at the bottom of the Scheduling Center page lists the scheduling events of the whole cluster, newest first:

| Column | Content |
|---|---|
| Time | When the event happened |
| Deployment | Opens that deployment; a deleted deployment is marked "deleted" |
| Action | The action name for the event code, such as automatic sleep, replica rebuilt, replicas changed |
| Reason and details | The same wording as the deployment timeline: from which node to which, which attempt, the schedule or idle time that triggered it, the replica count change, and so on |
| Category | **Automatic**: the platform acted on its own settings or schedules; **Failure**: something went wrong and the platform is dealing with it or has given up; **Manual**: an operator's order |
| By | Who gave a manual order; empty for automatic and failure events |

Filter by category, deployment (search by name), and time range (last hour / 24 hours / 7 days), and page with Older / Newer, 20 entries per page. The API is `GET /api/v1/scheduling/history`; see the [history API](../reference/api.md#调度记录).

What is in the history:

- **Automatic**: the actions of schedules, auto sleep and yielding cards, waiting wakes, and yielding wakes (see [Timeline Events](#时间线事件)), automatic placement of replicas, and replicas moved off a draining node.
- **Failure**: a failed replica rebuilt automatically (`REPLICA_REBUILT`), a unit that spent its retry budget and was given up, a deployment that cannot be placed, a rolling restart stopped on a replica that gave up, and replicas rescheduled after a node was lost.
- **Manual**: changing the replica count, restarting a deployment or a replica, retrying a replica, and ordering sleep and wake (`SLEEP_ORDERED`, `WAKE_ORDERED`). A manual event records the user who gave the order.

The history holds only these scheduling events; instance state changes, configuration edits, and the like are not in it and stay on each deployment's timeline.

## First Placement Nodes

When adding replicas to a single-node deployment, you can choose the nodes the new replicas start on first; see the note in [Deploy Models](deploy-models.md#部署后调整). This works alongside a scheduled replica count: the schedule changes only the number, and the candidate pool decides which node; for a multi-node deployment placed automatically the server picks the nodes.

## Timeline Events

Automatic actions are written to the deployment timeline (event kind `automation`, also shown in the [scheduling history](#调度记录)):

| code | Meaning | Main params |
|---|---|---|
| `AUTO_SLEEP` | A schedule, auto sleep when not preempted, or yielding cards put the deployment to sleep | `trigger` (`schedule` a schedule / `idle` auto sleep when not preempted / `preempt` yielding cards), `schedule_id`, `scheduled_at`, `level` (the level the platform chose, 1 or 2), `idle_sec`; when yielding cards also `reason` (`idle` idle for the full time / `rank` the other has a higher level / `turn` it is the other shared deployment's turn), `for_deployment` (the deployment it yields for), `abort` (whether in-flight requests were interrupted), `instances` |
| `AUTO_WAKE` | A schedule, a waiting wake, a request, or a resume woke instances | `trigger` (`schedule` / `wake_waiting` / `request` a request is waiting / `resume` taking cards back after yielding), `schedule_id`, `instances` |
| `AUTO_SCALE` | A schedule changed the replica count | `trigger`, `schedule_id`, `replicas`, `role` (PD) |
| `WAKE_WAITING` | A wake did not fit and now waits for room | `trigger` (`schedule` / `operator`), `instances`, `yielded` |
| `WAKE_WAIT_TIMEOUT` | The wait ended after 30 minutes | `trigger`, `instances`, `waited_sec` |
| `SCHEDULE_ACTION_FAILED` | An automatic action did not succeed and is not retried | `trigger`, `action`, `reason`, `reason_code`, `schedule_id`, `level`, `replicas`, `role` (PD) |
| `YIELDED_FOR_WAKE` | Recorded on the deployment put to sleep to make room: for whom, and to which level | `deployment`, `instances`, `level` |

An order an operator gives through the API is also written to the deployment timeline (event kind `schedule`) with `actor` (the user name):

| code | Meaning | Main params |
|---|---|---|
| `REPLICAS_CHANGED` | The replica count was changed | `from`, `to`, `role` (PD), `actor` |
| `SLEEP_ORDERED` | Sleep was ordered | `level`, `mode`, `scope`, `role`, `instances`, `actor` |
| `WAKE_ORDERED` | A wake started waking instances | `instances`, `actor` |

When a failed replica is rebuilt automatically the controller records `REPLICA_REBUILT` (params `replica`, `role`, `attempt`, `reason`, `node`); see the [history API](../reference/api.md#调度记录).

For the events of node-loss rescheduling after a node is lost and of drain moves (`NODE_REPLACED` and the like), see [Reschedule Replicas on Node Loss](node-failover.md#时间线事件).

## Limitations

- Cards are yielded only when the yield policy allows it: a never-yield deployment never sleeps for anyone else; a yielding wake still happens only when an operator names it and the occupant can sleep.
- No load-based scaling: the replica count changes only when a schedule entry is due.
- Yield policies other than never yield apply only to single-node deployments; when a new deployment starts and the cards are held by an awake lower-level deployment, the platform does not yield cards for the start, and the deployment waits as one that does not fit.
- A scheduled replica count applies only to single-node deployments and automatically placed multi-node and PD ones, not to multi-node or PD deployments placed by hand; a PD entry changes one role, so changing both takes two entries.
- The evaluator runs inside the server process; nothing happens automatically while the server is down, and after it recovers only the newest entry within 12 hours is made up.
