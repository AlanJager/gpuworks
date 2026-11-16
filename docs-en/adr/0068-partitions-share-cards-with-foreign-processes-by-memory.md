# ADR 0068: Memory-limited partitions share a card with processes outside the platform by the memory left

Date: 2026-11-16 · Status: Accepted (supplements [ADR 0060](0060-card-health-from-hardware-facts.md))

## Context

When a card has compute processes the platform did not start (a vLLM a user started, another orchestrator's container), the agent reports them as `foreign_processes` and the server treats the card as taking no deployment at all: its memory is held by something the platform can neither see nor plan around, and a whole-card deployment would run out of memory at startup.

A memory-limited dGPU partition (nvidia-dgpu and others) only ever wants part of a card, though, and the agent reports how much memory each of those processes holds. A real case: on a 143771 MiB H20-3e, another orchestrator's container held 63982 MiB, and a 50 GiB partition deployment was refused outright; after `foreign_processes` was cleared by hand, it ran stably beside that container on the same card.

Relaxing the server alone is not enough: the agent's rule for processes outside the platform is "while a card holds none of this node's instances, every process on it is foreign", and once a platform instance is on the card it stops examining it. The foreign processes vanish from the next report, the server takes the card for empty, and hands out what is left as if it were all of it.

## Decision

1. **Only partitions share, admitted by memory.** When a card is held by processes outside the platform, every one of their memory figures is known, the card is healthy and an operator has not stopped scheduling on it, a memory-limited deployment may be placed there: memory available = total − what the outside processes hold − headroom − what the platform's instances already claim (sleeping residue included), and it is admitted only if that covers the deployment's limit plus the partition overhead (`estimate.PartitionClaimBytes`). A whole-card deployment is still refused; `store.GPU.CanTakeWork` keeps its meaning, so card states, fleet statistics and SQL aggregates are unaffected.
2. **Headroom**: `estimate.ForeignHeadroomBytes` = 5% of the card, at least 4 GiB, the same shape as the host-memory headroom of an L1 sleep. What the outside processes hold is a reading taken once a minute, not a limit they keep to, and an out-of-memory failure costs both sides.
3. **Unknown means no sharing**: a process without a memory figure, a holder that is not a process (a card Kubernetes gave to another pod, which has no PID), or a list that cannot be read all count as unknown use, and the card stays refused.
4. **The agent remembers the outside processes**: once a platform instance is on the card, the agent keeps reporting the processes it had found to be foreign (matched by PID and process name), with their current memory, until they exit; after that the card is released after 3 free readings in a row, as before. A process that appears only after a platform instance is on the card is still not reported as foreign—the agent cannot tell it from its own instance's processes, and that coarse rule is unchanged. A round whose probe or process list fails restates the last answer rather than clearing it. The record is kept in the agent's state directory (`card-occupancy.json`) and survives an agent restart or upgrade; it lives only on the agent and is not a new persisted fact on the server.
5. **Gated by agent capability**: sharing is open only to an agent that does item 4, judged from the node's already-reported `partition_safety_protocol` level, refreshed by every report: `proto.PartitionSafetyForeignTracked` (2) means the agent keeps reporting outside processes when a partition shares their card. It is the next level of the same partition safety contract (level 1 is native vdev ownership); a higher level includes the lower ones, and rolling an agent back revokes it at the next report. No column is added.
6. **Sleep**: a wake onto a card held outside the platform is still refused; to be consistent, a deployment that sleeps is not placed on such a card either, refused with `held_outside_platform_sleep`, or once asleep it could never be woken there.
7. **Say why**: for a request that names its cards, the filters look only at the named cards, so a held card is reported as `gpus_held_outside_platform` rather than "no free GPU"; when memory is short, `no_fit` carries `foreign_bytes`/`foreign_gib` and the headroom parameters so the UI can explain the figures. Whether the card can be picked in the deploy form follows the server's per-card verdict for the current request (`gpu_candidates`); the frontend does not restate the rule.

## Trade-offs

- The agent still does not match processes to containers by cgroup: that would find outside processes that appear after a platform instance is on the card, but it needs PID-namespace handling per runtime and per vendor tool, and a wrong answer stops a card the platform is using. Sharing rests only on outside processes seen before the sharing began.
- PID plus process name may take a new process of the same name for the original one when a PID is reused; that only admits less, never more.
- The headroom is a fixed share rather than the outside processes' historical peak: a peak would need a new persisted fact, and 5%/4 GiB covers the reading fluctuation observed; an operator can still stop scheduling on the card.
- Nodes with an older agent cannot share until upgraded: better to ask for an agent upgrade than to take the card for free after the first placement.
