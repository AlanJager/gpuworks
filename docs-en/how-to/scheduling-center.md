# Use the Scheduling Center: Schedules, Idle Sleep, Waiting Wakes, and Service Levels

This guide explains how to have the platform carry out sleep, wake, and replica-count changes you agreed on in advance, and how to read the scheduling board to see who holds each card, what is waiting, and what is about to happen.

**Use case**: several sleep-capable models take turns on the same GPUs (for example one model by day and another at night), or a model sits unused for long periods and you want its GPU memory released automatically.

**Prerequisites**:

- Scheduled sleep, scheduled wake, and idle sleep require the deployment to be **sleep capable** (vLLM, enabled at creation); see [Sleep and Wake Operations](sleep-wake.md) for the conditions.
- A scheduled replica count applies only to single-node deployments (not PD, `nnodes` not above 1).
- Service levels other than resident apply only to sleep-capable single-node deployments.
- The scheduling center does not scale by load. The platform makes one deployment give up its cards for another only when service levels allow it; see [Service levels](#服务等级与按需唤醒).

## What the Scheduling Center Does

| Feature | What you agree in advance | What the platform does when due |
|---|---|---|
| Schedule | Weekdays, HH:MM, and time zone, plus sleep L1/L2, wake, or a replica count | Runs the same API action once at that time |
| Idle sleep | How long without user requests, and sleep to L1 or L2 | Sleeps automatically once the time is up |
| Waiting wake | Choose “wait” when a wake does not fit | Wakes automatically once cards free up; gives up after 30 minutes |
| Yielding wake | Choose “put the occupants to sleep, then wake” when a wake does not fit | Puts the occupants to sleep first, then wakes through waiting |
| On-demand wake | Nothing to set: deployments the platform put to sleep itself (idle sleep, yielding cards) | When a request arrives, the gateway holds it, wakes the deployment, and lets the request through once it is awake |
| Service level | Resident, online, shared, or offline | Deployments sharing a set of cards yield cards by level and take turns |

They carry out the same actions you would take by hand: the same lifecycle lock, the same eligibility checks and wake admission. An automatic wake and your manual **Wake** cannot both pass admission for the same card. Every automatic action is written to the deployment timeline; see [Timeline Events](#时间线事件).

The **Scheduling policy** card in deployment details keeps these settings in one place. Collapsed, it shows one summary line: the wait for node-loss rescheduling, the number of schedule entries and the next run, and the idle sleep setting (idle sleep is not shown when the deployment cannot sleep). Only admins see **Edit**; it opens one tab per setting, each saved on its own. For node-loss rescheduling, see [Reschedule Replicas on Node Loss](node-failover.md).

## Scheduling Board

Open the **Scheduling Center** page to see in one place:

- **Nodes and GPUs**: each card's total, the usage of awake instances, the residue of sleeping instances, and what is free; the occupants of the card (deployment, awake or asleep, sleep level, claim and residue); cards held by processes outside the platform are marked.
- **Deployments waiting for placement**: deployments the scheduler refused in the last 30 minutes, with reasons written the same way as the placement status in deployment details.
- **Waiting wakes**: which deployments are waiting and since when.
- **Schedule actions in the next 24 hours**: time, deployment, action.
- **Recent automatic actions**: the latest 50 automation timeline events.

Nodes are paged and can be searched by name. The API is:

```bash
curl --fail-with-body -sS \
  "$API_URL/api/v1/scheduling/board?page=1&page_size=20&q=gpu" \
  -H "Authorization: Bearer $TOKEN"
```

The main structure of the response is below (`nodes` is a paged object; the other four items cover the whole cluster):

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

The real `free_bytes` and the other sizes are integer bytes, and the values above are only illustrative; `residue_bytes` appears only on sleeping occupants.

## Schedules

Each deployment can have any number of entries, each of which is “do an action at a time on certain weekdays”. In the **Scheduling policy** card in deployment details, click **Edit** and add them on the **Schedule** tab:

1. Choose the action: **Sleep** (pick L1 or L2), **Wake**, or **Replica count** (1–32, single-node deployments only).
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

Request fields: `action` (`sleep` / `wake` / `replicas`), `weekdays` (0 for Sunday through 6 for Saturday, at least one, no repeats), `time` (`HH:MM`), `timezone` (an empty string uses the server's time zone), `sleep_level` (only for `sleep`, required), `replicas` (only for `replicas`, required), and `enabled` (default true).

### What Happens When an Occurrence Is Due

- **Each occurrence runs once.** The evaluator looks every 15 seconds and runs an entry when its newest occurrence is later than the one it last handled.
- **Catch-up**: for occurrences missed while the server was down, only the newest one runs after it starts, and that one may not be older than 12 hours; older ones are only marked handled, not run.
- **Manual operations are not overridden repeatedly**: an entry acts once at its time, and if you then wake or sleep by hand, the entry does not change it back on the next round.
- **A new or edited entry takes effect from its next occurrence** and does not look back at earlier ones.
- When two entries of one deployment are due together (the server was down across both), the earlier runs first and the newer runs last and stands.
- When an action fails (the deployment cannot sleep, it does not fit, and so on), the timeline records `SCHEDULE_ACTION_FAILED` and the occurrence still counts as handled; it is not retried every 15 seconds.
- **A scheduled wake that does not fit**: replicas that fit wake first and the rest go into a waiting wake (timeline `WAKE_WAITING`) rather than failing outright.

### Validation When Creating

| Case | Response |
|---|---|
| Missing fields, weekdays out of range or repeated, a bad time format, `sleep` without a level, `replicas` outside 1–32 | 400 `SCHEDULE_INVALID`; `params.field` and `params.reason` say what is wrong |
| The server does not know the time zone | 400 `SCHEDULE_TIMEZONE_UNKNOWN`, `params.timezone` |
| A replica count for a multi-node or PD deployment | 400 `SCHEDULE_REPLICAS_SHAPE` |
| The deployment is not sleep capable or the engine cannot sleep (`sleep` and `wake` actions) | 400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED` |

Multi-node and PD deployments list their replicas node by node, so a scheduled replica count is not offered for them.

## Idle Sleep

In the **Scheduling policy** card in deployment details, click **Edit** and use the **Idle sleep** tab to set how long without user requests before the deployment sleeps, and the sleep level. A suggested setting is 15 minutes and L2; enter 0 to turn it off.

```bash
curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/idle-sleep" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"after_sec":900,"level":2}'
```

Success returns 200 with the same `{"after_sec":900,"level":2}`; the `idle_sleep` field in deployment details gives the current setting.

- `after_sec` is 0 (off) or 60 to 604800 seconds (7 days), and `level` is 1 or 2, also when turning it off. Out of range returns 400 `IDLE_SLEEP_INVALID` (`params.field`); `after_sec` above 0 on a deployment that cannot sleep returns 400 `SLEEP_NOT_ENABLED` or `SLEEP_ENGINE_UNSUPPORTED`.
- The idle time counts from the latest of the server start, the last wake, and the last user request. After a server restart the deployment must therefore be watched for a whole idle period before it sleeps, and a deployment just woken is not put straight back to sleep.
- Health checks and other probes the platform sends itself are not user requests and do not reset the clock.
- When the sleep fails (for example, not enough host memory for L1), the timeline records `SCHEDULE_ACTION_FAILED`, and it tries again only after another whole idle period.
- A request that arrives after an idle sleep is held by the gateway, which wakes the deployment and lets the request through once it is awake; see [On-demand wake](#按需唤醒).

## Waiting Wakes and Yielding Wakes

When a wake is refused with `WAKE_INSUFFICIENT_GPU_MEMORY`, the console offers two choices, corresponding to `?wait=true` and `?yield=true`; for usage and responses see [Sleep and Wake Operations](sleep-wake.md#放不下时等位与让位). In brief:

- Waiting takes only room that is already there and makes no one give way; once capacity is released the server usually wakes the deployment within 15 seconds, and gives up after 30 minutes with `WAKE_WAIT_TIMEOUT` in the timeline.
- Yielding puts the occupants to sleep first; if an occupant cannot sleep, the whole request is refused with 409 `WAKE_YIELD_BLOCKED` and nothing changes.
- While waiting, the deployment details show a waiting banner and the start (`wake_waiting_since`), and you can click **Cancel waiting**, which maps to `DELETE /deployments/{id}/wake-waiting`. The start of the wait is stored in the database, so the wait continues after a server restart.
- When a process outside the platform holds the card, neither waiting nor yielding takes effect: the wake is refused outright with 409 `WAKE_GPU_HELD_OUTSIDE`.

## Service Levels and On-Demand Wake

A service level decides how a deployment gets along with the other deployments that share its cards. For the concepts and rules see [Service levels](../explanation/service-levels.md), and for the design trade-offs see [ADR 0059](../adr/0059-service-levels-and-the-residency-arbiter.md).

| Level | Suited to | Behavior |
|---|---|---|
| Resident (default) | Core online services | Never yields its cards |
| Online | Real-time services used intermittently | Yields only after 5 idle minutes; when it has requests, it makes shared and offline deployments yield at once |
| Shared | Several models taking turns on a set of cards | Takes turns with its peers, each turn lasting at least 2 minutes (longer when switching is slow); yields to online |
| Offline | Batch work, evaluation | Yields to everyone at any time, and in-flight requests are interrupted; resumes automatically once cards are free |

In the **Scheduling policy** card in deployment details, click **Edit** and choose on the **Service level** tab; you can also choose it directly when creating a deployment with **Sleep capable** on. The API:

```bash
curl --fail-with-body -sS -X PUT \
  "$API_URL/api/v1/deployments/7/service-level" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"level":"offline"}'
```

Success returns 200 with the same `{"level":"offline"}`. An invalid level returns 400 `SERVICE_LEVEL_INVALID`; anything other than resident requires sleep capability (400 `SLEEP_NOT_ENABLED` / `SLEEP_ENGINE_UNSUPPORTED`) and a single-node deployment (400 `SERVICE_LEVEL_SHAPE`); a deployment that shares cards with another online deployment cannot be set to online (409 `SERVICE_LEVEL_ONLINE_SHARED`, where `params.deployment` is the other one). The `service_level` field in deployment details gives the current level.

The typical use is real-time plus batch: set the real-time model to online and the batch model to offline, and deploy them on the same cards. After the real-time model has been idle for 5 minutes it yields its cards and the batch model resumes; when a real-time request arrives, the batch model yields at once and the request waits at the gateway for the real-time model to wake (an L1 switch takes about 2.5 seconds).

### On-Demand Wake

When the platform itself put a deployment to sleep (idle sleep, or yielding cards by service level), a request that arrives does not get a 503 from the gateway. The gateway holds the request, wakes the deployment, and lets the request through once it is awake:

- The hold time depends on the level: 2 minutes for resident and online, 10 minutes for shared, 30 minutes for offline; on timeout it returns 503 with `Retry-After`. Each deployment holds at most 256 requests, and any beyond that get 503 immediately.
- A sleep that you or a schedule issued is a **held sleep** (`sleep_held` is `true` in deployment details): requests do not wake it and still get 503. Any wake clears the hold.
- When it does not fit, the service levels decide whether the occupant yields; when it should not, the request waits until the hold time runs out.
- When an offline deployment yields its cards, its in-flight requests are interrupted: the client receives HTTP 200 with `finish_reason` set to `"abort"` and truncated content, and must retry on its own. A shared deployment first stops accepting new requests before yielding and waits up to 30 seconds for in-flight requests to finish.
- A deployment that yields its cards sleeps to L1 (a sleepable instance does one L1 prewarm when it starts, so the pinned host memory is already held; L2 when the host cannot hold the weights); shared and offline deployments resume automatically once cards are free. After a server restart, a deployment that had yielded its cards wakes only when a request arrives.

## First Placement Nodes

When adding replicas to a single-node deployment, you can choose the nodes the new replicas start on first; see the note in [Deploy Models](deploy-models.md#部署后调整). This works alongside a scheduled replica count: the schedule changes only the number, and the candidate pool decides which node.

## Timeline Events

Automatic actions are written to the deployment timeline (event kind `automation`, also shown under **Recent automatic actions** on the board):

| code | Meaning | Main params |
|---|---|---|
| `AUTO_SLEEP` | A schedule, idle sleep, or yielding cards put the deployment to sleep | `trigger` (`schedule` / `idle` / `preempt`), `schedule_id`, `scheduled_at`, `level`, `idle_sec`; when yielding cards also `reason` (`idle` idle for the full time / `rank` the other has a higher level / `turn` it is the other shared deployment's turn), `for_deployment` (the deployment it yields for), `abort` (whether in-flight requests were interrupted), `instances` |
| `AUTO_WAKE` | A schedule, a waiting wake, a request, or a resume woke instances | `trigger` (`schedule` / `wake_waiting` / `request` a request is waiting / `resume` taking cards back after yielding), `schedule_id`, `instances` |
| `AUTO_SCALE` | A schedule changed the replica count | `trigger`, `schedule_id`, `replicas` |
| `WAKE_WAITING` | A wake did not fit and now waits for room | `trigger` (`schedule` / `operator`), `instances`, `yielded` |
| `WAKE_WAIT_TIMEOUT` | The wait ended after 30 minutes | `trigger`, `instances`, `waited_sec` |
| `SCHEDULE_ACTION_FAILED` | An automatic action did not succeed and is not retried | `trigger`, `action`, `reason`, `reason_code`, `schedule_id`, `level`, `replicas` |
| `YIELDED_FOR_WAKE` | Recorded on the deployment put to sleep to make room: for whom, and to which level | `deployment`, `instances`, `level` |

For the events of node-loss rescheduling after a node is lost and of drain moves (`NODE_REPLACED` and the like), see [Reschedule Replicas on Node Loss](node-failover.md#时间线事件).

## Limitations

- Cards are yielded only when service levels allow it: a resident deployment never sleeps for anyone else; a yielding wake still happens only when an operator names it and the occupant can sleep.
- No load-based scaling: the replica count changes only when a schedule entry is due.
- Service levels apply only to single-node deployments; when a new deployment starts and the cards are held by an awake lower-level deployment, the platform does not yield cards for the start, and the deployment waits as one that does not fit.
- A scheduled replica count applies only to single-node deployments.
- The evaluator runs inside the server process; nothing happens automatically while the server is down, and after it recovers only the newest entry within 12 hours is made up.
