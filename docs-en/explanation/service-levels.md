# Yield Policy: How Deployments Share a Set of GPUs

This page explains what a yield policy is, the rule by which the platform makes one deployment give up its cards for another, and the trade-offs behind that rule. For the steps see [Use Scheduling Analysis](../how-to/scheduling-center.md#出让策略), and for the decision record see [ADR 0059](../adr/0059-service-levels-and-the-residency-arbiter.md).

## Why This Is Needed

A sleep-capable deployment leaves only a small residue on its cards once it sleeps, and the rest of the GPU memory can serve other deployments. Several deployments can therefore sit on the same cards with only some of them awake at any moment. The hard part is deciding who should be awake:

- A real-time chat model is used whenever someone shows up, while batch work should run whenever the cards are free; when a real-time request arrives, the cards should come back immediately.
- Several rarely used models take turns on a set of cards, and whichever has a request is awake, but they must not flap back and forth: a switch itself takes seconds, and switching too often leaves every model unable to serve.

One deployment expresses these two needs with one **yield policy** plus a few parameters that appear only when they apply. The platform enforces a single rule, and every action is written to the timeline. The yield policy is the only thing to choose.

While no other deployment waits for its cards, a deployment stays awake and does not put itself to sleep: sleeping on its own only leaves GPU memory empty and makes its own next request pay a wake. To sleep at fixed times, use a [schedule](../how-to/scheduling-center.md#定时计划).

## Yield Policy

The yield policy decides whether and when a deployment gives its cards to another deployment, that is, who may preempt whom. The rule looks at nothing else. The four policies are listed from the highest rank to the lowest:

| Policy | Suits | Meaning |
|---|---|---|
| Never yield automatically (`never`) | Always-on services | Never gives its cards to another deployment; only an operator or a schedule puts it to sleep |
| Yield when idle (`idle`) | Online services | Gives up its cards only when another deployment is waiting for them and it has been idle (no in-flight or held request) for the idle time; never yields while busy |
| Yield on a turn (`timeslice`) | Several models sharing in turns | Yields once idle for the idle time; yields at once to a higher-ranked deployment, busy or not; takes turns with other yield-on-a-turn deployments, each turn lasting at most the longest serving time |
| Yield anytime (`anytime`) | Batch and offline work | Yields once idle for the idle time; yields at once to a higher-ranked deployment, busy or not; yields to another yield-anytime deployment only when idle |

Policies other than never yield apply only to sleep-capable single-node deployments; a multi-node or PD deployment must use never yield.

### Parameters

The UI shows only the parameters that apply to the current policy. The API accepts inapplicable parameters, ignores them, and leaves them out of responses. A parameter left out takes the default of the chosen policy; the defaults are served by `GET /api/v1/scheduling/yield-policies`, and the UI keeps no copy.

| Parameter | Meaning | Applies to | Default (idle / turn / anytime) |
|---|---|---|---|
| `idle_sec` idle time (15–604800 s) | How long without a request counts as idle: once idle this long, the deployment yields if another one waits for its cards; for yield on a turn it may not exceed the longest serving time | Yield when idle, yield on a turn, yield anytime | 60 / 30 / 30 |
| `max_serve_sec` longest serving time (30–86400 s) | When taking turns with another yield-on-a-turn deployment, how long one wake may serve before giving way | Yield on a turn | — / 120 / — |
| `unfinished` handling of unfinished requests when yielding | `abort`: cut them off at once; `wait`: let them finish, for at most `unfinished_wait_sec` seconds (1–600, default 30), then cut off what is left | Yield on a turn and yield anytime (yield when idle yields only when idle, when nothing is running) | — / `wait` / `abort` |
| `sleep_level` sleep level | `auto`: L1, falling back to L2 when host memory cannot hold the weights; `l1`: strict L1, refused where the host cannot hold the weights, like an operator's L1 sleep; `l2`: L2. Applies to every sleep the platform chooses for the deployment (a yield); sleeps ordered by an operator or a schedule are unaffected. An operator's yielding wake with `yield_level=1` (the default) follows this `auto` rule too: a yield and a yielding wake make the same choice of sleep level | The three policies other than never yield | `auto` |
| `hold_max_sec` longest request hold (10–3600 s) | How long the gateway holds a request while the deployment is asleep or waking; on timeout it returns 503 with `Retry-After` | The three policies other than never yield | 300 / 600 / 3600 |

There is no unbounded wait: the longest is 600 seconds.

## The Yield Rule

A waiting deployment W can make an awake deployment H sleep if and only if H's policy is not never yield and at least one of the following holds:

1. **Idle**: H has no in-flight and no held requests, and has been idle for its idle time. This applies to all three policies that yield.
2. **Rank**: H is yield on a turn or yield anytime, and W ranks higher (never yield > yield when idle > yield on a turn > yield anytime). H yields even while busy, and the requests still running are handled by its `unfinished` setting.
3. **Turn**: H and W are both yield on a turn, and H has served for the longest serving time since it last woke.

Between two yield-anytime deployments only rule 1 applies: whichever is idle yields. The same holds for two yield-when-idle deployments.

When yields are counted, the residency arbiter putting a holder to sleep and an operator's yielding wake putting an occupant to sleep are the same kind of event: the overview, the timeline and the scheduling history count the same ones, and both record which deployment the cards were given up for.

If several awake replicas sit on the cards W needs, they either all yield or none does: yielding only some would stop service without freeing room. When several deployments wait at once, the higher rank goes first and, within the same rank, the one that has waited longest, so peers do not starve; a lower rank can be starved by design, for example batch work keeps waiting while the real-time model stays busy.

The rule is applied at the moment a yield falls due, not at a periodic evaluation: the evaluator keeps one timer for the next due moment (an idle time ending, a turn ending, a wait-to-finish limit running out), so thresholds of 15 or 30 seconds hold to about a second. For a busy holder that yields only once idle, the platform looks every 2 seconds whether it has become idle.

## Two Yield-When-Idle Deployments on One Card

Two yield-when-idle deployments may sit on the same card, but know how they behave: while both are busy neither yields, so one side's requests are held by the gateway until the other has been idle for its idle time, or until they reach their own `hold_max_sec` and get a 503. In the answer of `PUT .../yield-policy`, `idle_neighbours` lists the other yield-when-idle deployments on the card, and the UI shows a warning from it. To make them take turns, use yield on a turn.

## One Yield, Step by Step

1. A request arrives at a sleeping deployment. The gateway holds the request and notifies the residency arbiter (handled within 100 ms).
2. The arbiter first tries a normal wake. If it fits, it wakes the deployment, and the gateway releases the request once the deployment is awake.
3. If it does not fit, the arbiter judges the holder by the rule above. To yield:
   - If the holder yields because it is idle, it has nothing running and goes to sleep at once.
   - Otherwise the holder's `unfinished` setting decides what happens to the requests still running: `abort` puts it to sleep at once and cuts them off; `wait` first has the gateway stop sending it new requests (new requests are held as well) and puts it to sleep once the in-flight requests finish, waiting at most `unfinished_wait_sec` seconds and cutting off whatever is still running at that point. A holder that finished everything first sleeps with `mode=wait`; one cut off at the time limit sleeps with `mode=abort`.
4. Once the holder is confirmed asleep and only its residue remains on the cards, the waiter is woken and the held requests are released. While a yield lands, the arbiter looks every 250 milliseconds, for up to two minutes, and only at the waiting wakes of the deployment the yield was for; that deployment's wake lands within one look of the holder going to sleep.
5. Yield-on-a-turn and yield-anytime deployments that gave up their cards are recorded as pending resume and take the cards back under the same rule when they are free again.

An interrupted non-streaming request is held by the gateway and replayed once the deployment is back (up to 3 times), so the client sees only the finished result. A streaming request has already started to produce output and cannot be replayed: the client sees a normal HTTP 200 whose `finish_reason` is `"abort"` and whose content is truncated, and should retry accordingly. Long jobs can go to the platform-managed [batch jobs](../how-to/batch-api.md#与随时出让搭配), which retries interrupted requests automatically.

## Why Platform-Chosen Sleeps Default to L1

Measured on an L20 (vLLM 0.17.1, 7B and 14B models): with two models swapping on one card, the time from the yielding side going to sleep to the woken side emitting its first token was about 2.5 seconds with L1; with L2 it was about 3.5–5 seconds when the model files stayed in the page cache, and 40–110 seconds for the 14B model when they had to be read from disk. The first L1 sleep of an engine pins host memory of about 1.9 times the weight size and does not release it after waking; the platform also runs one L1 warm-up after every sleep-capable instance starts (hiding a first-sleep cost of over ten seconds), so this memory is already held from startup. L1 therefore takes no more host memory, switches fastest, and does not depend on the page cache.

So the sleep level defaults to `auto`: L1 first, falling back to L2 when available host memory cannot hold the weights (the estimate is conservative). Choose `l1` for strict L1 (refused where it does not fit, instead of quietly getting slower), or `l2` to trade switch time for host memory.

This also means: **a sleep-capable deployment holds host memory of about 1.9 times its weight size from startup.** When several sleep-capable deployments run on one host, plan its host memory by that amount.

## Held Sleep

A sleep ordered by an operator or a schedule is a held sleep: a request does not wake it, and the gateway still answers 503, so a request cannot overturn the operator's decision. A sleep the platform made itself (yielding cards) is not held and a request wakes it; a held sleep does not keep its cards either (see [How Placement Leaves Room for Sharing](#放置怎样为共卡留位置)). Any wake clears the hold, including a wake that has to wait for room: when an operator's `POST /deployments/{id}/wake?wait=true` answers 202 `waiting`, the hold is cleared at once. The deployment is meant to be awake, so a request that arrives afterwards is held by the gateway for that wake, and the residency arbiter may make room for it under the yield policies, instead of getting a 503; before, the hold stayed until the wake landed.

## How Placement Leaves Room for Sharing

A deployment that shares cards by yield policy (any policy other than never yield) keeps its cards while asleep only if **a request can wake it**: then deployments it does not share with (never yield) still count its full claim, so no deployment lands on its cards and leaves a request unable to wake it; for the other deployments that share cards by yield policy only the residue counts and they may be placed there. A request can wake it when the deployment is sleepable and the sleep is not a [held sleep](#保持休眠) (the deployment's `Deployment.WakesOnRequest`; the gateway, the residency arbiter and placement all ask this same question).

A sleep ordered by an operator or a schedule (a held sleep) **keeps no card**: a request does not wake it, so its cards are open to any new deployment, and the later wake by the operator or the schedule either waits for room (`?wait=true`) or makes it by having the holders sleep (`?yield=true`), see [Sleep and Wake Operations](../how-to/sleep-wake.md#放不下时等位与让位).

When a new deployment does not fit only because sleepers keep their cards, the reason in the waiting-for-placement list is `reserved_for_sleepers`, with the parameters `node`, `gpus` (the cards it would have used) and `sleepers` (the sleepers, each with `deployment` and `policy`, its yield policy). The UI reads: "GPU 0 of gpu3 is reserved for the sleeping qwen3-asr (yield when idle) so a request can wake it; a deployment that never yields is not placed on these cards. Pick another card, turn on sleep for this deployment with a yield policy that can share cards, or put the deployment holding the cards to sleep by hand (a deployment put to sleep by hand keeps no card)." The yield policy can already be chosen when a deployment is created, and placement and preview are computed from its policy.

## How a New Replica Waits for Room

When a deployment that shares cards by yield policy (any policy other than never yield) starts a new replica, the cards it needs may be held awake by other deployments that share them. The replica then no longer stays unplaceable: placement plans it again with the awake instances of the deployments that share cards with it (whose policy is not never yield either) counted as asleep; if it fits that way, the replica waits for them to give up the cards, and the pending reason is `waiting_for_yield`. The reason names the node, the terms on which each holder deployment gives way (`holders`: `at_once` gives way at once, or after `idle_sec` idle or `max_serve_sec` served in this turn), and the holding instances. The UI reads, for example: "Waiting: batch yields the GPU at once; shared yields the GPU after 30 seconds idle or 2 minutes served this turn. Then the replica is placed on gpu-1."

- **The residency arbiter treats the replica like a request held for a sleeping deployment**, with the policy of the deployment the replica belongs to, and puts the holders to sleep by [the same yield rule](#让卡规则): when the newcomer ranks higher, yield-on-a-turn and yield-anytime holders give way at once; otherwise the holder gives way once it has been idle for its idle time; between two yield-on-a-turn deployments it gives way when the holder has served its longest serving time this turn. The replica is placed and started only after the holders are asleep and only their residue is left on the cards.
- **A new deployment with never yield waits for nobody** and still needs free cards; a holder with never yield is never asked to give way.
- While it waits, the controller plans the replica again every 5 seconds. When a deployment has several candidate nodes (a pool), a node that needs a yield is taken only when no node fits outright. Once it waits on a node, each look is at that node alone first: if the replica still fits once its holders sleep, only that node is planned again, and the rest of the pool is searched only when it no longer fits even with the holders' yield.
- A holder that gave way does not take the cards back while the new replica is being placed and started (up to 10 minutes); the new replica's turn and idle time count from when it begins to serve.
- The wait lives in server memory only: after a server restart the controller notes it again on its first pass.
- Measured (process test): an online (yield-when-idle) deployment created onto a card an offline (yield-anytime) deployment held awake started serving about 6 seconds after creation, and the offline one was put to sleep by rank.

## Limitations

- Policies other than never yield cover single-node deployments only; a multi-node or PD deployment must use never yield.
- A new replica waiting for holders to give up cards applies only to single-node deployments that share cards by yield policy (see the section above); a multi-node or PD deployment uses never yield and still waits as unplaceable when its cards are held.
- Held requests, each wake's start time, and the pending-resume list all live in server memory: after a server restart, a deployment that yielded its cards is woken only when a request arrives.
- Requests interrupted on yield-anytime and yield-on-a-turn deployments must be retried by the client; the platform-managed [batch jobs](../how-to/batch-api.md#与随时出让搭配) (`/v1/batches`) retry automatically until the job expires.
- A deployment does not sleep on its own while no other deployment waits for its cards. A deployment that had "auto sleep when not preempted" turned on before the upgrade no longer sleeps on its own when nobody waits for its cards; to sleep at fixed times, use a [schedule](../how-to/scheduling-center.md#定时计划).
