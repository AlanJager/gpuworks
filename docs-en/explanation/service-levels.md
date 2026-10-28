# Service Levels: How Deployments Share a Set of GPUs

This page explains what a service level is, the rule by which the platform makes one deployment give up its cards for another, and the trade-offs behind that rule. For the steps see [Use the Scheduling Center](../how-to/scheduling-center.md#服务等级与按需唤醒), and for the decision record see [ADR 0059](../adr/0059-service-levels-and-the-residency-arbiter.md).

## Why This Is Needed

A sleep-capable deployment leaves only a small residue on its cards once it sleeps, and the rest of the GPU memory can serve other deployments. Several deployments can therefore sit on the same cards with only some of them awake at any moment. The hard part is deciding who should be awake:

- A real-time chat model is used whenever someone shows up, while batch work should run whenever the cards are free; when a real-time request arrives, the cards should come back immediately.
- Several rarely used models take turns on a set of cards, and whichever has a request is awake, but they must not flap back and forth: a switch itself takes seconds, and switching too often leaves every model unable to serve.

A service level expresses these two needs as one attribute of a deployment. The platform enforces a single rule, and every action is written to the timeline.

## The Four Levels

| Level | Yielding cards | Taking cards |
|---|---|---|
| Resident (default) | Never yields | Once put to sleep by the platform, can make shared and offline deployments yield when it has requests |
| Online | Yields only after 5 idle minutes | Can make shared and offline deployments yield at once |
| Shared | Yields at any time to online and resident; takes turns with its peers, each turn lasting at least 2 minutes | Can make offline deployments yield; peers go by waiting order |
| Offline | Yields to everyone at any time, and in-flight requests are interrupted at once | Takes only free cards, and resumes automatically once cards are free |

Two online deployments cannot share a card: when both are busy neither yields, and each one's requests wait on the other forever. The platform rejects this when the level is changed, and placement never puts an online deployment on the cards of another online deployment. Levels other than resident apply only to sleep-capable single-node deployments.

## The Yield Rule

A waiting deployment W can make an awake deployment H sleep if and only if H is not resident and at least one of the following holds:

1. **Idle**: H has no in-flight and no held requests, and has been idle for the yield time of its level (5 minutes for online, 1 minute for shared, 10 seconds for offline).
2. **Level**: H can be preempted (shared or offline), and W has a higher level.
3. **Turn**: H and W are both shared, H has stayed for its minimum residency this turn, and W's oldest request has waited 30 seconds. The minimum residency is the larger of 2 minutes and 10 times the switch time, so that the switching cost stays a small fraction of serving time.

If the cards W needs hold several awake replicas, they either all yield or none does: yielding only some would stop service without freeing any room. When several deployments wait at once, the higher level goes first, and within a level the one that has waited longest goes first, so peers are not starved; a lower level can by design be starved, for example batch work waits for as long as the real-time model stays busy.

## How One Yield Goes

1. A request arrives at a sleeping deployment; the gateway holds it and notifies the residency arbiter (handled within 100 milliseconds).
2. The arbiter first tries a normal wake; if it fits, it wakes the deployment, and the gateway lets the held requests through once it is awake.
3. If it does not fit, the arbiter judges the occupant by the rule above. If it must yield: an offline deployment sleeps at once and its in-flight requests are interrupted; a shared deployment first stops accepting new requests (new requests are held as well) and waits up to 30 seconds for in-flight requests to finish, and interrupts any still running at that point; an idle one simply sleeps.
4. Once the occupant is confirmed asleep and only its residue remains on the cards, the waiting deployment is woken and the held requests are let through.
5. A shared or offline deployment that yielded is recorded as pending resume, and takes the cards back under the same rule once they are free again.

To the client an interrupted request ends normally: HTTP 200, `finish_reason` set to `"abort"`, and truncated content. Batch clients should retry on that signal.

## Why Yielding Uses L1

Measured on an L20 (vLLM 0.17.1, 7B and 14B models): swapping two models on the same card, from the yielding side going to sleep to the woken side producing its first token, took about 2.5 seconds with L1; with L2 it took about 3.5–5 seconds while the model files stayed in the page cache, and 40–110 seconds for 14B when they had to be read from disk. An engine's first L1 sleep pins host memory of about 1.9 times the weight size and does not release it after the wake; the platform does one L1 prewarm right after every sleepable instance starts (hiding the first sleep's tens of seconds), so that memory is already held from the start. An L1 yield therefore takes no more host memory, switches fastest, and does not depend on the page cache. When the host's available memory cannot hold the weights (a conservative estimate), the yield falls back to L2.

This also means that **a sleepable deployment holds about 1.9 times its weight size in host memory from the moment it starts**. Plan host memory by that amount when one host runs several sleepable deployments.

## Held Sleep

A sleep issued by an operator or a schedule is a held sleep: requests do not wake it, and the gateway still returns 503, so that a request cannot overturn an operator's decision. A sleep the platform made itself (idle sleep, yielding cards) is not held, and a request wakes it. Any wake clears the hold.

## How Placement Leaves Room for Sharing

While a deployment that shares cards by service level is asleep, it is still counted at full usage against deployments it does not share with (resident ones, or another online one when it is online), so no deployment lands on its cards and leaves it unable to wake; against shared and offline deployments only its residue is counted, and they can be placed there. You can choose the level when creating a deployment, and placement and preview both compute from it.

## Limitations

- Only single-node deployments are covered; multi-node and PD deployments stay resident.
- When a new deployment starts and the cards are held by an awake deployment, the platform does not yield cards for the start; the new deployment waits as one that does not fit, and is placed once the other yields or sleeps.
- Held requests, each turn's residency start, and the pending-resume list all live in server memory: after a server restart, a deployment that had yielded its cards wakes only when a request arrives.
- Requests interrupted on an offline deployment need client retries; platform-hosted batch jobs are not yet supported.
