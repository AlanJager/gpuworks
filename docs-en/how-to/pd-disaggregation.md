# PD Disaggregation Deployment

**Use cases**: With long prompts (thousands of tokens) and high concurrency, prefill competes with decode and makes output unstable. Split inference into separate prefill and decode instance groups, optionally across machines.

After completing this guide, you can call the model through a single service name and confirm from handoff events and request details that prefill participated in the request.

**Prerequisites**:

- You are familiar with ordinary deployment. See [Deploy Models, Select GPUs, and Interpret Scheduling Results](deploy-models.md).
- Your hardware and engine meet the requirements of the selected PD mode: `vllm-nixl` targets NVIDIA, `vllm-ascend-mooncake` targets Ascend, and `sglang-nixl` is unverified.
- In NVIDIA mode, KV travels over the compute network when P and D are on different machines; RDMA or Ethernet can be used. In Ascend mode, cross-machine deployments require a shared accelerator RDMA network, and the NPUs hosting prefill and decode must be mutually reachable over the NPU network. For configuration and validation, see [Configure Node Networks](configure-networks.md).

PD disaggregation splits inference for one model into two instance groups: prefill instances process only the prompt, and decode instances retrieve their KV and then generate tokens one by one. Externally, it remains
one deployment with one service name. Send requests to `/v1` as usual; the gateway coordinates the two instance groups. For the design and tradeoffs, see
[ADR 0028](../adr/0028-pd-disaggregated-deployment.md).

## When to Use It

- Benefits are greatest with long prompts (thousands of tokens) and high concurrency when prefill competes with decode and destabilizes output. For very short prompts, KV transfer overhead may outweigh
  the benefits. The mean prompt length on the service page's PD card can help you decide.
- For cross-machine NVIDIA deployments, compare RDMA and Ethernet handoff overhead. Do not apply the conditions that allow Ethernet to Ascend mode.
- In NVIDIA mode, P and D can use separate cards on the same node to verify functionality, but this does not represent cross-machine performance.

## Deploy

1. On the deployments page, select **PD disaggregation** under **Node Placement**, then select a PD mode. Available modes come from the engine profiles: vLLM offers `vllm-nixl` (NVIDIA) and
   `vllm-ascend-mooncake` (Ascend); SGLang offers `sglang-nixl` (unverified).

2. Set TP separately for prefill and decode, then add instances one by one. Select a node for each instance and optionally pin cards. Each mode has TP rules: `vllm-nixl` requires
   decode TP ≥ prefill TP, `vllm-ascend-mooncake` requires the reverse, and `sglang-nixl` requires equal values.
   When creating a deployment, each of the P and D roles needs at least one instance.

3. To span an instance across multiple nodes, select **Add Node** under that instance. The first node is the leader (serving external requests), and the rest are followers. You must specify TP in this case,
   with TP × PP divided evenly among the nodes. The platform waits until the model and image are ready on every node before starting all ranks together.

4. Select the failure handling policy for the mode so that you do not mistake a successful request for a successful PD handoff:

   | Mode | When P Is Unavailable | When Decode Fails to Retrieve KV |
   |---|---|---|
   | `vllm-nixl`, `vllm-ascend-mooncake` | Decode completes locally by default (`fallback: decode`); returning 503 (`fail`) is also available | Returns an error by default (`kv_load_failure: fail`); local recomputation (`recompute`) is also available |
   | `sglang-nixl` | Only returning 503 is supported; decode cannot perform prefill independently | This setting is unavailable |

   Mode, TP, PP, and KV load failure policy cannot be changed after startup; clone the deployment to change them. Confirm these selections before submitting.

5. In Ascend mode, KV uses the NPUs' built-in RoCE ports. For cross-machine deployment, these nodes must share an accelerator RDMA network (see
   [Configure Node Networks](configure-networks.md)); otherwise, the deployment is rejected. The NPUs hosting prefill and decode must also be mutually reachable over the NPU network. Use
   `hccn_tool -i <card-index> -lldp -g` to inspect each port's peer, and `hccn_tool -i <card-index> -ping -g address <peer-ip>` to verify connectivity.

   If two machines are directly connected by card number without a switch, P and D must select matching card numbers that are actually connected.
   First run **Card reachability · full card matrix** under the network's **Internal test** to check cabling. Deployment validation does not verify card-to-card links for you.

6. The preview shows placement, GPU memory, and complete commands for each instance. KV Offload applies only to prefill; decode does not attach a KV cache layer. Do not include
   `--kv-transfer-config` or `--kv-offloading-size` in custom arguments; the platform rejects them.

7. Submit the deployment. In its details, confirm that both P and D roles reach their desired ready counts, then check the handoff self-check result in the prefill instance timeline.
   Send a request through Playground or `/v1/chat/completions`, then check the prefill instance and fallback reason in request details.
   For the two vLLM modes with sequential handoff, you can also see the “PD prefill” phase. If a fallback reason appears, a successful response does not confirm that disaggregation took effect.

API form: `placement.pd` in `POST /api/v1/deployments`. See the [API Reference](../reference/api.md) for fields.

## During Operation

### Status

The deployment shows **Not ready / Partially ready / Running**, derived from each role's desired and ready counts. Prefill intentionally scaled to 0 is not counted as missing.

### Self-Check

After each prefill instance first becomes ready, the platform automatically performs one handoff through the gateway and records
`PD_HANDOFF_VERIFIED` in that instance's timeline (transport is rdma, tcp, local, or unmeasured). If the compute network is RDMA but no RDMA traffic is observed, it records
`PD_HANDOFF_NOT_RDMA`. If the handoff fails, it records `PD_HANDOFF_FAILED`.

### Scale

Open **Scale roles** in deployment details to change the instances for each role (`PUT /api/v1/deployments/{id}/pd`). Removed instances drain before stopping.

- Both vLLM modes support scaling prefill to 0. Requests are then intentionally handled entirely by decode and do not count as fallback.
  This differs from still expecting P instances but falling back or returning 503 according to `fallback` when P fails.
- `sglang-nixl` requires instances to remain for both P and D.

### Retry an Instance

Once an instance reaches the consecutive failure limit, the platform stops recreating it. Deployment details show its failure count and latest reason and provide
**Retry**, which clears only that instance's failure record and recreates it without affecting other instances. Removing an instance under **Scale roles** also clears its failure record;
adding it again starts a new count.

### Image and Network Changes

New or recreated instances start with the current image and compute network. Differences from running instances are not blocked (for example, replacing images one at a time for a
canary rollout, or cutting over a network). If the two sides are incompatible or unreachable, handoffs fail, as reflected in self-checks (`PD_HANDOFF_FAILED`) and the fallback rate. Restart the entire group if needed.

### Restart

**Restart** in the deployment details page header (`POST /api/v1/deployments/{id}/restart`, available for all deployment types) recreates all instances using the currently resolved configuration and resets each instance's
retry count. By default, instances are replaced one at a time (other instances continue serving, and the next replacement starts only after the previous instance recovers). Selecting **All at once** stops all instances and recreates them together, interrupting service during the process.

### Monitoring

The service page's PD card provides:

- **PD Effectiveness**: The proportion of decode prompt tokens received through KV transfer. The denominator includes tokens from transfer, local computation, and cache hits.
  A low value alone does not prove handoff failure. Consider the desired P count, sleep state, fallback reasons, and handoff self-check results together.
- **Fallback rate and top reasons**: The ratio of fallback requests to gateway requests, aggregated by reason. In the two vLLM modes, when P is intentionally scaled to 0 or all P instances are asleep,
  decode handles requests independently and they do not count as fallback.
- **Mean and P95 prefill phase duration** (the gateway's wait for the prefill instance), prefill instance queueing, and mean prompt length;
- **KV transfer**: Mean / P95 transfer time, mean size, throughput, failure count, and expiry count, from the vLLM NIXL connector's `vllm:nixl_*` metrics. These are per-transfer statistics,
  not statistics for individual requests.

  Failures count unsuccessful KV pulls by decode. Expiries count requests whose computed KV the prefill instance had to release because it was not retrieved before the lease expired (30 seconds by default in vLLM 0.30, continuously renewed after decode receives
  the request). This indicates that the request did not reach
  decode after prefill (the client disconnected or handing it to decode failed), or that the decode instance failed.

  The `vllm-ascend-mooncake` connector does not export these metrics, so the card shows “Not provided by engine.” Individual transfer times appear only in decode instance logs
  (`KV cache transfer for request … took … ms`).

### Requests

#### View Details and Colored Bars

The PD section of request details shows the prefill instance, prefill phase, that instance's reported queue and Prefill times, and the fallback reason;
requests still in the prefill phase also appear in the in-flight table. For field definitions, see [Request Timeline and Phase Breakdown](../reference/request-timeline.md#6-pd-分离部署).

Both vLLM modes use sequential handoff. The request view lists a separate “PD prefill” segment after “Gateway”; the same segment is simply called “Prefill” in the end-to-end latency breakdown. The decode instance's reported queue time appears as
“KV transfer & queue” (both vLLM and vllm-ascend include the wait for remote KV in queue time), and its reported Prefill appears as “First-token recompute.” In the latency breakdown,
this segment is hidden if it accounts for 1% or less of mean end-to-end latency.

#### Identify Cache Hits

Cache hits for sequential handoff requests (and `cached_tokens` returned to the client) use the prefill instance's value, because decode also counts retrieved KV as cache hits. See [OpenAI-Compatible Gateway](../reference/openai-gateway.md#用量usage).

#### View Traces

In exported OTel traces for sequential handoff, the prefill phase is a `pd.prefill` child span.
`sglang-nixl` calls P and D concurrently, and the gateway does not wait for P's response, so there is no separate `pd.prefill` child span.

### Evaluation

By default, evaluations measure end-to-end performance through the gateway. If you specify an instance, they measure that instance directly, and results represent only one ordinary replica.

## Troubleshooting

- **High fallback rate**: Check the gateway's counted reasons (`prefill_unreachable`, `prefill_5xx`, `prefill_no_kv_params`, and others) and prefill instance logs.
  `prefill_no_kv_params` usually means the prefill KV connector did not take effect.
- **An instance was ready, but requests later keep failing or falling back**: First check instance events and logs. The agent periodically checks running containers;
  if a container exits or disappears, it is treated as failed and the controller recreates it according to the retry policy. Use **Retry** when the retry limit is reached.
  This check is not continuous HTTP health probing. If the container is still running but the engine does not respond, check logs and request errors, then restart or replace the instance through role adjustment if necessary.
