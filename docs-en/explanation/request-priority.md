# Request Priority

Request priority makes the engine queue requests by priority: higher-priority requests are scheduled first, and when GPU memory (KV cache) runs short, lower-priority requests are preempted first. It relies on the priority scheduling built into vLLM and SGLang; the platform turns on the engine's switch and lets only the gateway decide each request's priority.

## The Switch on a Deployment

The **Request priority** field of the deployment form is ticked by default. Evaluation judges whether it can be turned on from the arguments the engine finally receives (the inference template's arguments after the custom arguments override them), and create and edit check the same rule. When it is on, the platform adds to the engine:

| Engine | Arguments the platform injects | The engine's direction |
|---|---|---|
| vLLM | `--scheduling-policy priority` | Lower values run first; ties go by arrival |
| SGLang | `--enable-priority-scheduling --default-priority-value 0` | Higher values run first; requests without a priority (health checks, the engine's own warmup) count as 0 |

When the custom arguments already contain an argument of the same name, the platform does not inject it and the user's value stands. Changing the switch rebuilds replicas; the edit confirmation lists `priority_scheduling` as a change that needs a restart. Deployments created before the upgrade stay off; turn it on in an edit when needed.

It cannot be turned on in the following cases; the deployment form gives the reason next to the switch, and the deployment does not distinguish priorities:

| Reason | Details |
|---|---|
| The engine is not vLLM or SGLang | Other engines have no priority scheduling the platform can drive |
| The engine build for the accelerator is unverified | Turned on for NVIDIA (vLLM, SGLang; measured), AMD (vLLM, SGLang) and Hygon (vLLM); the latter two run the same scheduler code as upstream, confirmed by source and not yet measured on a card. Ascend, PPU, Kunlunxin, Hygon SGLang and others stay off for now |
| PD disaggregated deployment | Not supported yet |
| Conflicting scheduling arguments | vLLM's `--scheduling-policy` is not `priority`; SGLang's `--schedule-policy` is not `fcfs` or `lof` (for example `lpm`), or `--schedule-low-priority-values-first` is set |

When the conflict comes from the inference template (some templates use `--schedule-policy lpm` for prefix-cache hits), the switch offers one click: it adds `--schedule-policy fcfs` to the custom arguments, overriding the template's value, and evaluation then judges again. Doing so loses the original policy's optimization. When the conflict comes from the user's own arguments, the user changes them.

## How the Gateway Handles Priority

Clients cannot decide a priority themselves. On every `/v1` request the gateway:

- removes the `X-Vllm-Priority` header (vLLM uses it to override the priority in the body);
- sets a top-level `priority` in a JSON body to `0`. Both engines read 0 as the neutral level; vLLM rejects `null`, so the field is not emptied. A body without `priority` is forwarded as sent, without copying it.

Both stages of a PD request are handled the same way.

## Effects and Limits

- Priority only matters while the engine has a queue. When the engine does not queue, turning it on makes no difference.
- In vLLM, a new high-priority request does not displace running requests; it waits for a slot under `--max-num-seqs`. Only when KV cache runs short is the lowest-priority, latest-arriving request preempted. A preempted streaming request pauses for a few seconds and then continues with complete output.
- In SGLang, a new request whose priority exceeds a running request's by more than 10 (the default `--priority-scheduling-preemption-threshold`, strictly greater) preempts it at once; the preempted request resumes from the prefix cache.
- Both engines use strict priority with no aging by waiting time: under sustained saturation, low-priority requests can wait without bound.
- Priority only takes effect inside one replica; the gateway does not consider it when choosing among replicas.
- Measured on a single NVIDIA card, turning it on changed throughput, time to first token and prefix-cache hit rate for same-priority load by no more than measurement noise.
