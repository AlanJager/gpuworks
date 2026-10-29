# Yield Policy and Service Levels: How Deployments Share a Set of GPUs

This page explains what a yield policy is, how the four service levels are presets of it, the rule by which the platform makes one deployment give up its cards for another, and the trade-offs behind that rule. For the steps see [Use the Scheduling Center](../how-to/scheduling-center.md#服务等级与休眠), and for the decision record see [ADR 0059](../adr/0059-service-levels-and-the-residency-arbiter.md).

## Why This Is Needed

A sleep-capable deployment leaves only a small residue on its cards once it sleeps, and the rest of the GPU memory can serve other deployments. Several deployments can therefore sit on the same cards with only some of them awake at any moment. The hard part is deciding who should be awake:

- A real-time chat model is used whenever someone shows up, while batch work should run whenever the cards are free; when a real-time request arrives, the cards should come back immediately.
- Several rarely used models take turns on a set of cards, and whichever has a request is awake, but they must not flap back and forth: a switch itself takes seconds, and switching too often leaves every model unable to serve.
- A model that nobody competes with for cards but that nobody uses for a long time can also put itself to sleep to save GPU memory and power.

One deployment expresses these three needs with one **yield policy** plus a few parameters that appear only when they apply. The platform enforces a single rule, and every action is written to the timeline.

## Yield Policy

The yield policy decides whether and when a deployment gives its cards to another deployment, that is, who may preempt whom. The rule looks at nothing else.

| Policy | Meaning | Rank |
|---|---|---|
| Never yield (`never`) | Never gives its cards to another deployment. Once put to sleep by the platform, it can take cards from deployments that yield when idle, take turns, or yield anytime when it has requests | Highest |
| Yield when idle (`idle`) | Gives up its cards only when another deployment is waiting for them and it has been idle (no in-flight or held request) for the idle time | Can preempt time slice and anytime |
| Time slice (`timeslice`) | Takes turns with the other time-slice deployments on the same cards: each keeps them for at least the minimum serving time, and the waiter's oldest request must have waited 30 seconds; yields at once to never-yield and yield-when-idle waiters; also yields to any waiter once idle for the idle time | Can preempt anytime |
| Yield anytime (`anytime`) | Gives its cards to any higher-ranked waiter at once; to a waiter of the same policy only after it has been idle for a fixed 30 seconds (not configurable) | Lowest |

Two yield-when-idle deployments cannot share a card: when both are busy neither yields, and each one's requests wait on the other forever. The platform rejects this when the policy is changed, and placement never puts a yield-when-idle deployment on the cards of another one. Policies other than never yield apply only to sleep-capable single-node deployments; a multi-node or PD deployment must use never yield (but may turn on auto sleep).

### Parameters

The UI shows only the parameters that apply to the current policy. The API accepts inapplicable parameters, ignores them, and leaves them out of responses.

| Parameter | Meaning | Applies to |
|---|---|---|
| `idle_sec` idle time (30–604800 s) | How long without a request counts as idle | Yield when idle and time slice (once idle this long, yield if someone waits); any policy when auto sleep is on |
| `auto_sleep` auto sleep when not preempted | Sleeps once idle for the idle time even when no other deployment waits for its cards. This is a sleep the platform chose, not a held sleep, and a later request wakes it | Every sleep-capable deployment, including multi-node and PD |
| `sleep_level` sleep level | `auto`: L1, falling back to L2 when host memory cannot hold the weights; `l1`: strict L1, refused where the host cannot hold the weights, like an operator's L1 sleep; `l2`: L2. Applies to every sleep the platform chooses for the deployment (a yield, an auto sleep); sleeps ordered by an operator or a schedule are unaffected | A policy other than never yield, or auto sleep on |
| `min_serve_sec` minimum serving time (0–3600 s) | How long a deployment keeps the cards after waking before an equal may take them; the residency arbiter raises it to 10 times the measured switch time when that is larger | Time slice |
| `unfinished` handling of unfinished requests when yielding | `abort`: cut them off at once; `wait`: let them finish, for at most `unfinished_wait_sec` seconds (1–600), then cut off what is left; `wait_unbounded`: let them finish however long it takes | Time slice and yield anytime (yield-when-idle yields only when idle, when nothing is running; never yield never yields) |
| `hold_max_sec` longest request hold (10–1800 s) | How long the gateway holds a request while the deployment wakes; on timeout it returns 503 with `Retry-After` | A policy other than never yield, or auto sleep on |

Be careful with `wait_unbounded`: one long request (a long generation, for example) can keep the waiter waiting indefinitely, and the waiter's requests are held at the gateway for at most its own `hold_max_sec` and then get a 503. When unsure, use the bounded `wait`.

## The Four Service Levels Are Presets

The four service levels are presets: a yield policy together with a full set of parameters.

| Preset | Policy | Parameters |
|---|---|---|
| Resident (default) | Never yield | Auto sleep off |
| Online | Yield when idle | Idle time 300 s, sleep level `auto`, longest request hold 120 s, auto sleep off |
| Shared | Time slice | Idle time 60 s, minimum serving time 120 s, unfinished requests wait to finish (at most 30 s), sleep level `auto`, longest request hold 600 s, auto sleep off |
| Offline | Yield anytime | Unfinished requests aborted at once, sleep level `auto`, longest request hold 1800 s, auto sleep off |

A preset is not stored separately: the deployment stores only the policy and parameters. The preset shown in the UI and the API is derived by comparing the stored policy and parameters with the four presets item by item. An exact match is that preset, and changing any item (for example setting an online deployment's idle time to 30 seconds, or turning on auto sleep for a resident one) makes the deployment **custom**. Choosing a preset fills in its whole set of parameters.

## The Yield Rule

A waiting deployment W can make an awake deployment H sleep if and only if H's policy is not never yield and at least one of the following holds:

1. **Idle**: H has no in-flight and no held requests, and has been idle for its yield idle time (`idle_sec` for yield when idle and time slice, a fixed 30 seconds for yield anytime).
2. **Rank**: H can be preempted (time slice or yield anytime), and W ranks higher (never yield > yield when idle > time slice > yield anytime).
3. **Turn**: H and W are both time slice, H has held its cards for at least the minimum serving time in this turn, and W's oldest request has waited 30 seconds. The minimum serving time is the larger of `min_serve_sec` and 10 times the switch time, so switching costs only a small share of the serving time.

If several awake replicas sit on the cards W needs, they either all yield or none does: yielding only some would stop service without freeing room. When several deployments wait at once, the higher rank goes first and, within the same rank, the one that has waited longest, so peers do not starve; a lower rank can be starved by design, for example batch work keeps waiting while the real-time model stays busy.

## One Yield, Step by Step

1. A request arrives at a sleeping deployment. The gateway holds the request and notifies the residency arbiter (handled within 100 ms).
2. The arbiter first tries a normal wake. If it fits, it wakes the deployment, and the gateway releases the request once the deployment is awake.
3. If it does not fit, the arbiter judges the holder by the rule above. To yield:
   - If the holder yields because it is idle, it has nothing running and goes to sleep at once.
   - Otherwise the holder's `unfinished` setting decides what happens to the requests still running: `abort` puts it to sleep at once and cuts them off; `wait` and `wait_unbounded` first have the gateway stop sending it new requests (new requests are held as well) and put it to sleep once the in-flight requests finish. `wait` waits at most `unfinished_wait_sec` seconds and cuts off whatever is still running at that point; `wait_unbounded` waits however long it takes. A holder that finished everything first sleeps with `mode=wait`; one cut off at the time limit sleeps with `mode=abort`.
4. Once the holder is confirmed asleep and only its residue remains on the cards, the waiter is woken and the held requests are released.
5. Time-slice and yield-anytime deployments that gave up their cards are recorded as pending resume and take the cards back under the same rule when they are free again.

An interrupted non-streaming request is held by the gateway and replayed once the deployment is back (up to 3 times), so the client sees only the finished result. A streaming request has already started to produce output and cannot be replayed: the client sees a normal HTTP 200 whose `finish_reason` is `"abort"` and whose content is truncated, and should retry accordingly. Long jobs can go to the platform-managed [batch jobs](../how-to/batch-api.md#与离线服务等级搭配), which retries interrupted requests automatically.

## Why Platform-Chosen Sleeps Default to L1

Measured on an L20 (vLLM 0.17.1, 7B and 14B models): with two models swapping on one card, the time from the yielding side going to sleep to the woken side emitting its first token was about 2.5 seconds with L1; with L2 it was about 3.5–5 seconds when the model files stayed in the page cache, and 40–110 seconds for the 14B model when they had to be read from disk. The first L1 sleep of an engine pins host memory of about 1.9 times the weight size and does not release it after waking; the platform also runs one L1 warm-up after every sleep-capable instance starts (hiding a first-sleep cost of over ten seconds), so this memory is already held from startup. L1 therefore takes no more host memory, switches fastest, and does not depend on the page cache.

So the sleep level defaults to `auto`: L1 first, falling back to L2 when available host memory cannot hold the weights (the estimate is conservative). Choose `l1` for strict L1 (refused where it does not fit, instead of quietly getting slower), or `l2` to trade switch time for host memory.

This also means: **a sleep-capable deployment holds host memory of about 1.9 times its weight size from startup.** When several sleep-capable deployments run on one host, plan its host memory by that amount.

## Held Sleep

A sleep ordered by an operator or a schedule is a held sleep: a request does not wake it, and the gateway still answers 503, so a request cannot overturn the operator's decision. A sleep the platform made itself (auto sleep, yielding cards) is not held and a request wakes it. Any wake clears the hold.

## How Placement Leaves Room for Sharing

When a deployment that shares cards by yield policy (any policy other than never yield) is asleep, deployments it does not share with (never yield, or, for a yield-when-idle deployment, another yield-when-idle one) still count its full claim, so no deployment lands on its cards and leaves it unable to wake; for time-slice and yield-anytime deployments only the residue counts and they may be placed there. The preset can already be chosen when a deployment is created, and placement and preview are computed from its policy.

## Limitations

- Policies other than never yield cover single-node deployments only; a multi-node or PD deployment must use never yield, and auto sleep is not affected.
- When a new deployment starts while its cards are held by an awake deployment, the platform does not make that one yield for the start; the new deployment waits as unplaceable until the other yields or sleeps.
- Held requests, each turn's start time, and the pending-resume list all live in server memory: after a server restart, a deployment that yielded its cards is woken only when a request arrives.
- Requests interrupted on time-slice and yield-anytime deployments must be retried by the client; the platform-managed [batch jobs](../how-to/batch-api.md#与离线服务等级搭配) (`/v1/batches`) retry automatically until the job expires.
- The evaluator checks the conditions for auto sleep and yielding every 10 seconds, so the actual sleep happens at most one tick after the idle time (a yield triggered by a request is handled within 100 ms).
