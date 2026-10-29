# Sleep and Wake Operations

This guide explains how to put an entire deployment, selected replicas, or a PD role to sleep to release GPU memory, then wake them when needed and verify that service resumes.

**Use case**: multiple models take turns using a GPU, and temporarily unused instances need to release GPU memory while keeping their containers. Sleep discards request history in the KV cache; wake does not restore previous request state.

**Prerequisites**:

- The deployment uses vLLM and has **Sleep Capable** enabled at creation. Single-node and regular multi-node replicas are supported; SGLang is not.
- A PD deployment can sleep when KV transfer uses Ethernet. If KV transfer uses host RDMA networking, sleep capability cannot be enabled (`PD_SLEEP_RDMA_UNSUPPORTED`). This restriction does not apply to regular multi-node deployments using RDMA for collective communication.
- L1 requires enough host memory to hold the offloaded weights. If the available host memory minus headroom cannot hold them, the platform refuses L1 (409 `SLEEP_HOST_MEMORY_INSUFFICIENT`); choose L2 instead and ensure that the original model directory remains readable when waking.
- For the complete procedure for deploying multiple models on the same GPU, see [Run Two Models on One GPU](../tutorials/one-gpu-two-models.md). For boundaries such as custom images, see [Limitations](#限制).

## Two Levels

| Level | On Sleep | On Wake | Suitable When |
|---|---|---|---|
| L1 (fast) | Moves weights to host memory and discards the KV cache | Restores weights from memory and allocates an empty KV cache | Host memory is sufficient |
| L2 (deep) | Discards weights without writing them to disk; discards the KV cache | Reloads weights from the original model directory, restores KV cache space, and clears the prefix cache | Host memory is insufficient and the weight-reloading delay is acceptable |

The KV cache stores keys and values for tokens in request history. Sleep discards this content; wake allocates only empty cache space. Prefix caching reuses previously computed common prefixes through the KV cache. L2 clears old reuse records after reloading weights; L1 retains weights in host memory and restores them directly.

L2 has no setting to choose which disk to use. Wake still reads the model directory mounted by the deployment. If NFS is unavailable, rereading may stall or fail.

## What Happens to In-Flight Requests During Sleep

Choose an option in the sleep confirmation page:

- **Finish in-flight requests first (recommended)** (default): the engine finishes requests it is processing before sleeping. Long requests delay sleep; the agent allows up to 10 minutes per command.
- **Abort in-flight requests now**: requests being processed immediately receive an error.

Once the command is sent, the gateway immediately stops forwarding new requests to the instances selected for sleep. Other awake replicas that can serve requests continue receiving them. Only when no awake replica can serve requests does the gateway return HTTP 503 with the message `model is asleep; wake it first (console, or POST /api/v1/deployments/<id>/wake; a schedule or a waiting wake may also wake it)`. A sleep you issue by hand is a held sleep, and the gateway does not wake it because of a request; a schedule or a waiting wake can wake it for you. A sleep the platform made itself (yielding cards by yield policy) is woken automatically when a request arrives; see the [Scheduling Center](scheduling-center.md#按需唤醒).

## Put an Entire Deployment to Sleep

1. Open deployment details and select L1 or L2 **Sleep**.
2. In the confirmation page, choose to wait for completion or abort immediately, then confirm.
3. Wait for the deployment's sleep label to show the selected level. Instances remain `running`, and `sleep_level` becomes 1 or 2. If only some complete, inspect each replica's status and failure reasons in the timeline. Sending the command does not mean that all instances have successfully gone to sleep.

The API is `POST /api/v1/deployments/{id}/sleep?level=1|2&mode=wait|abort`.

## Partial Sleep: Replicas and PD Roles

In a deployment with multiple replicas, you can put some to sleep while others continue serving:

| Scope | Entry Point and Conditions |
|---|---|
| One replica | Click **Sleep replica (L1)** or **Wake replica** in the replica leader's instance panel. With only one replica, use the deployment-level button. A multi-node replica sleeps and wakes as a whole; you cannot operate on a follower individually |
| One PD role | Click **Sleep Prefill (L1)** in deployment details. This is offered only for a role that the PD mode allows to be empty. For vLLM NIXL, this is prefill: when all prefill replicas sleep, decode performs prefill, and this does not count as degraded operation. Decode can sleep only with the entire deployment; the last awake decode replica cannot be put to sleep individually either (`SLEEP_ROLE_CANNOT_EMPTY`) |

The single-replica APIs are `POST /api/v1/instances/{uuid}/sleep?level=&mode=` and `POST /api/v1/instances/{uuid}/wake`; role sleep uses `POST /api/v1/deployments/{id}/sleep?role=prefill`.

After confirming, check the status of the selected scope. Deployment labels count replicas; partial sleep displays **Partly asleep · k/N replicas**.

Sleep state after recreation depends on the sleep scope:

- **Entire deployment asleep**: newly created instances and instances recreated after failure also start at that level.
- **Individual replica or role asleep**: they are awake after recreation.

## Host Memory Check Before L1 Sleep

L1 moves weights into host memory. Before sleeping, the platform estimates the weights to move for the instances about to sleep on each node (each rank of a multi-node replica counts its share) and compares them with the node's most recently reported available memory. If the need exceeds the available memory minus the headroom, it refuses; the headroom is the larger of 5% of total host memory and 4 GiB. The refusal is 409 `SLEEP_HOST_MEMORY_INSUFFICIENT` with the parameters `node`, `need_gib`, and `available_gib`; use L2 instead. A node with no recent host memory observation is not judged, and the decision is left to you.

## What the GPU Can Do After Sleep

A sleeping instance holds only its **residue** on each GPU (the CUDA context, allocator pools, the CUDA graph pool, and communication buffers on multiple GPUs); the rest of the GPU memory can go to other deployments. The residue is recorded per instance and is no longer a fixed constant:

- **Measured**: when the instance is alone on its cards and no process outside the platform is on them, the agent reads the card's used memory about 1 second after a successful sleep and uses it as this instance's residue.
- **Estimated**: when the card is shared with other instances, or nothing can be read, the platform estimates from the model. A model of up to 3B, 30B, 200B parameters, or larger takes 0.75, 1, 1.5, or 2.5 GiB; add 0.5 GiB for the CUDA graph pool (assumed on); add 0.5 GiB when the engine spans more than 1 card, or 1 GiB from 8 cards; the result is limited to 0.75–4 GiB. An unknown model size counts as 1.5 GiB. The parameter count is derived from the weight bytes and the stored precision.

Without a measurement the estimate is used; the residue is never treated as 0. A measurement is kept until the instance's next sleep measures again. Followers in a multi-node replica are accounted for according to the leader's sleep state.

If deployment evaluation finds insufficient space, follow the page's GPU memory release suggestion and click **Sleep and Reevaluate**. Available GPU memory in the evaluation already subtracts the residue.

The capacity released is really released: once another deployment fills the card, a sleeping instance may not fit back when it wakes. This is stated behavior (409 `WAKE_INSUFFICIENT_GPU_MEMORY`), not a defect.

Why a sleeping instance does not keep its original allocation: sleep would then be pointless for several models taking turns on one card, and the memory you meant to hand over would stay locked. The price is that a wake is not guaranteed; if you need that, give those deployments cards of their own. A measurement reads the whole card's used memory, which runs high, so it errs on the safe side; an estimate is only a rule of thumb and may be high or low; a sleeping instance that shares its cards with other instances always uses the estimate.

## Wake

1. Click **Wake** in deployment details or the replica panel. The deployment-level API is `POST /api/v1/deployments/{id}/wake`.

   Waking any replica revokes the deployment-wide sleep command. Other sleeping replicas remain asleep.

2. If you receive 409 `WAKE_INSUFFICIENT_GPU_MEMORY`, inspect the GPU index, shortfall in GiB, and occupants shown in the message. You can stop or put those occupants to sleep before retrying, or let the platform do it, see [Wait or Yield When It Does Not Fit](#放不下时等位与让位). By default, a deployment-level wake proceeds only if all replicas fit. After a wake request for multiple sleeping replicas is rejected, you can choose **Wake what fits** (`?partial=true`). A multi-node replica is evaluated as a whole: if any rank cannot fit back into memory, the entire replica remains asleep.
   When a process outside the platform holds the card (the platform cannot see how much it uses), the wake is refused with 409 `WAKE_GPU_HELD_OUTSIDE`, with the same parameters as above and `refused[].held_outside` set to true. Release that process on the node first, then wake; waiting and yielding cannot get around this. The agent also examines cards whose platform instances all sleep: when the sleeping engine's own process IDs are known, it can tell them apart from processes outside the platform.
3. Wait for `sleep_level` to return to 0 for the selected replicas, then retry a `/v1` request to confirm service recovery. For a partial wake, also check the remaining replicas' GPU memory shortfalls. One successful request does not establish that all replicas are awake.

Waking instances are already accounted for at their full GPU memory usage to prevent allocating the same GPU memory twice while waiting for the agent's report.

### Wait or Yield When It Does Not Fit

After a wake is refused with `WAKE_INSUFFICIENT_GPU_MEMORY`, the console offers two choices, which correspond to two API parameters:

| Approach | Entry Point | Effect |
|---|---|---|
| Wait | **Wait for room**, `?wait=true` | Replicas that fit wake first; the rest wait for room, and the server wakes them automatically once capacity frees up; it gives up after 30 minutes and records this in the timeline |
| Yield | **Put the occupants to sleep, then wake**, `?yield=true`, optionally `&yield_level=1\|2` (default 1) | First puts the awake deployments holding the needed cards to sleep, then places the wake into waiting; it completes automatically once they are asleep |

```bash
curl --fail-with-body -sS -X POST \
  "$API_URL/api/v1/deployments/7/wake?wait=true" \
  -H "Authorization: Bearer $TOKEN"
```

When everything fits, the response is 200 `{"status":"waking","woken":[...],"waiting":[],"yielded":[]}`; if some must wait it is 202 `{"status":"waiting",...}`, where `waiting` lists the instances waiting and `yielded` the names of the deployments put to sleep to make room. For a single instance use `POST /api/v1/instances/{uuid}/wake?wait=true`.

Details of yielding:

- Occupants sleep at L1 by default; when the host memory of their node cannot hold L1, the platform switches to L2 automatically. Use `yield_level=2` to ask for L2 directly.
- If any occupant cannot sleep (a deployment that is not sleep-capable, the last awake role of a PD deployment, or a card held by a process outside the platform), the whole request is refused with 409 `WAKE_YIELD_BLOCKED` (`occupants` lists the names) and nothing changes.
- The deployment put to sleep records `YIELDED_FOR_WAKE` in its timeline; unless a schedule says otherwise, it does not wake up by itself.

Once waiting starts, the hold is cleared: the deployment is meant to be awake, so a request that arrives while it waits is held by the gateway for that wake, and the residency arbiter may make room for it under the yield policies, instead of getting a 503 (before, the hold stayed until the wake landed).

While a deployment waits, its details page shows a waiting banner and the start time (the deployment's `wake_waiting_since`). To give up waiting, click **Cancel waiting**, or call:

```bash
curl --fail-with-body -sS -X DELETE \
  "$API_URL/api/v1/deployments/7/wake-waiting" \
  -H "Authorization: Bearer $TOKEN"
```

It returns 200 `{"items":["<instance UUID>"],"total":1}` listing the instances whose wait was cancelled. Waiting and yielding, together with schedules and yield policies, make up the [Scheduling Center](scheduling-center.md); the scheduling board shows who is waiting.

If sleep or wake fails, first inspect the instance timeline:

When one instance fails to sleep, only its own desired state and any still-active deployment-wide command are rolled back. Other replicas already asleep are not woken with it. Then choose the next action based on whether the actual state can be confirmed:

| Result | Next Action |
|---|---|
| Actual sleep state can be confirmed | The platform preserves the confirmed actual sleep level and failure reason, and rolls back only the desired state still associated with this failed command to that level. Resolve the cause, then click the corresponding button again; failed commands are not retried automatically |
| State cannot be confirmed, or an L2 wake fails after partially allocating GPU memory | The instance is marked failed, continues to account for full GPU memory usage, and is blocked from gateway forwarding. The platform removes and recreates this instance generation. Wait for the recreation result; do not estimate available GPU memory using the sleep residual |

## Limitations

- Only vLLM is supported. SGLang cannot sleep (declaring sleep capability is rejected at creation).
- A PD deployment cannot declare sleep capability when KV transfer uses host RDMA networking (`PD_SLEEP_RDMA_UNSUPPORTED`): NIXL registers KV cache GPU memory with the RDMA NIC,
  and after engine sleep releases this memory, wake cannot allocate it again (CUDA out of memory occurs in testing). PD deployments with KV transfer over Ethernet and regular multi-node deployments with collective communication over RDMA can sleep.
- Custom images are not probed; the engine's declared capabilities are authoritative. If the image lacks a sleep endpoint, sleep reports an error in instance events.
- The engine's sleep endpoint is enabled by `VLLM_SERVER_DEV_MODE=1`, on the condition that the engine listens only locally and is accessed through the agent tunnel.
  Reassess this requirement if the deployment architecture changes.
- Stopping an instance differs from sleep: stop actually stops and removes the running container. Click **Start** in deployment details (`POST /api/v1/deployments/{id}/start`) to recover.
