# Sleep and Wake Operations

This guide explains how to put an entire deployment, selected replicas, or a PD role to sleep to release GPU memory, then wake them when needed and verify that service resumes.

**Use case**: multiple models take turns using a GPU, and temporarily unused instances need to release GPU memory while keeping their containers. Sleep discards request history in the KV cache; wake does not restore previous request state.

**Prerequisites**:

- The deployment uses vLLM and has **Sleep Capable** enabled at creation. Single-node and regular multi-node replicas are supported; SGLang is not.
- A PD deployment can sleep when KV transfer uses Ethernet. If KV transfer uses host RDMA networking, sleep capability cannot be enabled (`PD_SLEEP_RDMA_UNSUPPORTED`). This restriction does not apply to regular multi-node deployments using RDMA for collective communication.
- L1 requires enough host memory to hold the offloaded weights. If host memory is insufficient, choose L2 and ensure that the original model directory remains readable when waking.
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

Once the command is sent, the gateway immediately stops forwarding new requests to the instances selected for sleep. Other awake replicas that can serve requests continue receiving them. Only when no awake replica can serve requests does the gateway return HTTP 503 and `model is asleep; wake it first`.

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

## What the GPU Can Do After Sleep

The scheduler accounts for a sleeping instance as retaining approximately 1.5 GiB per GPU; the remaining GPU memory is available to new deployments. This is a scheduling estimate, not a guarantee of actual usage. Followers in a multi-node replica are accounted for according to the leader's sleep state.

If deployment evaluation finds insufficient space, follow the page's GPU memory release suggestion and click **Sleep and Reevaluate**. Available GPU memory in the evaluation already subtracts the residual usage and identifies its source.

## Wake

1. Click **Wake** in deployment details or the replica panel. The deployment-level API is `POST /api/v1/deployments/{id}/wake`.

   Waking any replica revokes the deployment-wide sleep command. Other sleeping replicas remain asleep.

2. If you receive 409 `WAKE_INSUFFICIENT_GPU_MEMORY`, inspect the GPU index, shortfall in GiB, and occupants shown in the message. Stop or put those occupants to sleep before retrying. By default, a deployment-level wake proceeds only if all replicas fit. After a wake request for multiple sleeping replicas is rejected, you can choose **Wake what fits** (`?partial=true`). A multi-node replica is evaluated as a whole: if any rank cannot fit back into memory, the entire replica remains asleep.
3. Wait for `sleep_level` to return to 0 for the selected replicas, then retry a `/v1` request to confirm service recovery. For a partial wake, also check the remaining replicas' GPU memory shortfalls. One successful request does not establish that all replicas are awake.

Waking instances are already accounted for at their full GPU memory usage to prevent allocating the same GPU memory twice while waiting for the agent's report.

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
