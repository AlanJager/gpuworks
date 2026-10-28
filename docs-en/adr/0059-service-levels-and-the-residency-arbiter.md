# ADR 0059: Share GPUs by Service Level: the Gateway Holds Requests While a Wake Happens, and the Residency Arbiter Preempts by One Yield Rule

Date: 2026-10-28 · Status: Accepted

Terminology follows `CONTEXT.md`: **service level**, **residency arbiter**, **yield**, **held sleep**.
Changes two points of decision 1 in [ADR 0058](0058-scheduling-center-acts-through-operator-orders.md): “no preemption” and “the gateway does not hold requests while waiting for a wake”;
the rest (schedules, idle sleep, waiting wakes, yielding wakes, sleep releasing capacity down to a residue) is unchanged.

## Background

ADR 0058 only had the platform carry out orders operators gave in advance. In practice the two most common ways of sharing cards are these: real-time chat is used whenever someone shows up while batch work runs whenever the cards are free;
or several models are each rarely used and take turns on a set of cards. The former needs “take the cards back as soon as real-time arrives, and hand them back to batch when it goes idle”, the latter needs “take turns, but do not flap”.
Schedules and manual yielding cannot do this: nobody knows in advance when a request will arrive, and a request to a sleeping deployment only gets a 503.

Measured on an Alibaba Cloud L20 on 2026-10-28 (vLLM 0.17.1, model files in the page cache): swapping a 7B and a 14B model on the same card, from the yielding side going to sleep to the woken side producing its first token took
about 2.5–5 seconds; an L1 sleep pins host memory of about 1.9 times the weight size and does not release it after the wake, which pushes the other model's files out of the page cache and turns its wake into a disk read (40–110 seconds for 14B);
`/sleep?mode=abort` returns in 0.7 seconds, and in-flight requests end normally with `finish_reason: "abort"` (HTTP 200); the engine does not reject requests that arrive during sleep, but holds them until after the wake.

## Decision

**1. Every deployment has a service level: resident, online, shared, or offline.** Existing deployments are all read as resident, and their behavior is unchanged. Levels other than resident apply only to sleep-capable single-node deployments.

| Level | Yielding cards | Taking cards |
|---|---|---|
| Resident | Never yields | Once put to sleep by the platform, can make shared and offline deployments yield when it has requests |
| Online | Yields only after 5 idle minutes | Can make shared and offline deployments yield at once |
| Shared | To online and resident at any time; takes turns with its peers, each turn lasting at least Q | Can make offline deployments yield; peers go by waiting order |
| Offline | To everyone at any time, and in-flight requests are interrupted at once | Takes only free cards |

Two online deployments cannot share a card: when both are busy neither yields, and each one's requests wait on the other forever. Changing a level is rejected in that case (409 `SERVICE_LEVEL_ONLINE_SHARED`), and placement never puts an online deployment on the cards of another online deployment.

**2. There is exactly one yield rule** (`servicelevel.Yields`): a waiting W can make an awake H sleep if and only if H is not resident and at least one of the following holds:

- H is idle (no in-flight and no held requests) for the yield time of its level (5 minutes for online, 1 minute for shared, 10 seconds for offline);
- H can be preempted (shared or offline) and W has a higher level;
- H and W are both shared, H has stayed for Q this turn (Q = max(2 minutes, 10 × the measured switch time)), and W's head request has waited 30 seconds.

If the cards a waiter needs hold several awake replicas, they either all yield or none does: yielding only some would stop service without freeing any room. Waiters queue by level and then by how long the head request has waited, so peers are not starved.

**3. The gateway holds requests for a deployment the platform put to sleep.** When a request reaches a sleeping deployment whose sleep was made by the platform (idle sleep, an arbiter yield),
the gateway holds the request, notifies the arbiter, and picks an instance again every time the routes are rebuilt; holding has limits on count (256 per deployment, 4096 in total) and on time (2 minutes for resident and online, 10 minutes for shared, 30 minutes for offline),
and on timeout it returns 503 with `Retry-After`. A sleep issued by an operator or a schedule is a **held sleep** (`deployments.sleep_held`): requests do not wake it and still get 503, so that a request cannot overturn an operator's decision.
Although the engine itself holds requests that arrive during sleep, the gateway does not forward requests to a sleeping engine: a wake must pass admission, and when it does not fit, the engine would hang half awake.

**4. The residency arbiter is one step of the automation evaluator** (`internal/server/automation/residency.go`), run on every tick and within 100 milliseconds after the gateway holds a request.
It first tries a normal wake (the same wake admission and waiting as the API); when that does not fit, it judges the occupants by rule 2, and once it decides to yield: offline sleeps at once with `mode=abort`;
a busy shared deployment first stops accepting new requests (the gateway holds new requests as asleep) and waits up to 30 seconds for in-flight requests to finish, aborting any still running then; an idle one sleeps with `mode=wait`.
A sleep from yielding is always L2 and is not held; preemptible levels (shared, offline) are recorded as “pending resume” and take the cards back by rule 2 when they are free. Every yield and wake is written to the timeline
(`AUTO_SLEEP` with `trigger: preempt`, `reason`, and `for_deployment`; `AUTO_WAKE` with `trigger: request | resume`).

**5. Placement leaves room for sharing.** While a deployment that shares cards by service level (anything other than resident) is asleep, it is still counted at full usage against levels it does not share with (resident, or another online one when it is online),
so no deployment lands on its cards and leaves it unable to wake; against shared and offline only the residue is counted, and they can be placed there. The level can be declared at creation (`service_level`), and placement and preview compute from it.

**6. Trade-offs on state.** Persistence adds only two columns: `deployments.service_level` (text, one of four values) and `deployments.sleep_held` (boolean).
Held requests, in-flight counts, each deployment's residency start this turn, the pending-resume list, and drain progress live only in memory: after a server restart, a deployment that had yielded its cards wakes only when a request arrives.

## Consequences

- The two sharing patterns, real-time plus batch and several models taking turns, no longer need operators to arrange them by hand; every action still maps one-to-one to a timeline entry.
- When an offline deployment is preempted, its in-flight requests are truncated: the client sees HTTP 200 with `finish_reason: "abort"` and must retry on its own; platform-hosted batch jobs (`/v1/batches`) come later.
- Yielding is always L2: the switch is about 1 second slower than L1, in exchange for not pinning host memory and not pushing other models out of the page cache; when the model files are not in the page cache, an L2 wake is a disk read, taking the weight size divided by the disk speed.
- The first version covers only single-node deployments; multi-node and PD stay resident. When a new deployment starts and the cards are held by an awake lower-level deployment, the first version does not preempt for the start, and the deployment waits as one that does not fit, to be placed once the other yields or sleeps.
- The arbiter and the gateway counts live in memory and are read per deployment without scanning all deployments; levels affect only the few deployments that share cards by service level, and the path for resident deployments is unchanged.
