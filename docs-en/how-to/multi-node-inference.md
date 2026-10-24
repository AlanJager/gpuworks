# Multi-Node Inference

**Use cases**: The model does not fit on one node, or you want to distribute TP / PP across nodes so that multiple ranks of a deployment run on different machines.

After completing this guide, all ranks of a replica are ready, and you can complete an inference request using the deployment's service name.

**Prerequisites**:

- At least two online nodes, connected as described in [Connect Nodes](connect-nodes.md).
- All nodes must share a compute network. Without a shared compute network, fallback to the management network is possible only if every node allows it to carry compute traffic. For network selection and RDMA settings, see [Configure Node Networks](configure-networks.md).
- NVIDIA uses NCCL/UCX for communication on a host RDMA compute network. This network cannot be used on runveil (udocker driver) nodes.
- Ascend multi-node inference requires an accelerator RDMA network.
- You are familiar with single-node deployment. See [Deploy Models, Select GPUs, and Interpret Scheduling Results](deploy-models.md).

## Deploy a Multi-Node Replica

1. Check each node's address on the selected network and confirm that firewalls allow bidirectional TCP between these addresses.
   On the management network, each address is the local address the node uses to connect to the server. On Ascend nodes, also verify connectivity between the accelerator cards you actually select.
   To verify links, select these nodes and run an [internal network test](configure-networks.md#内部测试);
   TCP reachability does not prove RDMA or accelerator network availability. Select the corresponding test method.
   If a network probe fails, fix routes, firewalls, or ports first. Do not bypass the failure by disabling authentication.
2. On the deployments page, fix the node order by rank 0…N-1 and configure parallelism. Tensor parallelism (TP) and pipeline parallelism (PP)
   apply to the entire deployment. Total cards = TP × PP; each node runs TP × PP ÷ node count cards (the division must be exact).
   After you select nodes, TP defaults to the smallest GPU count among selected nodes, and PP defaults to the node count. TP can also span nodes: for example, two nodes with
   1 card each can use TP 2. A node with more GPUs than the per-node card count uses only some of them.
   In the multi-node form, parallelism settings follow nodes and precede the image. TP / PP errors appear on the corresponding fields and do not prevent image and weight acquisition estimates from appearing.
3. Select the image and weights, then expand the per-node acquisition estimates, such as “1 of 2 nodes needs to pull the image.”
   Nodes that need resources follow the [pull settings](configure-pull-settings.md); nodes that already have them are skipped. Image pre-pulling before startup uses the same candidates.
4. Review the form's configuration assessment. Correct errors in nodes, shared networks, and TP / PP, and confirm that the master port is available.
   The form precheck validates only configuration and node conditions. It does not measure network connectivity or bandwidth.
   For a separate API probe, call `POST /api/v1/deployments/preflight`. TCP bandwidth is measured only if
   `bandwidth` is enabled in the request. See the [API Reference](../reference/api.md) for fields and results.
5. Check node order and parallelism in the command preview, then submit the deployment. vLLM uses `--tensor-parallel-size` / `--pipeline-parallel-size`,
   and SGLang uses `--tp-size` / `--pp-size`. The corresponding PP argument is injected only when PP is greater than 1. The rank 0 address serves as master, and external traffic enters only rank 0;
   the other ranks work as followers, so you do not need to call their endpoints individually.
6. Wait for all ranks to become ready in deployment details, then send a request through Playground or `/v1` using the deployment's service name.
   Seeing rank 0 created or beginning to load does not mean the entire replica is available. On failure, inspect the affected rank's events and logs.

If the deployment keeps waiting for nodes, check `DEPLOYMENT_UNSCHEDULABLE` in the timeline. It lists the ranks that cannot be placed, their nodes, and the reasons.
All ranks of a replica must simultaneously meet the model, image, port, GPU, and GPU memory requirements before they are created together;
the leader does not reserve GPUs first, and this wait does not consume replica retries. Once you supply the missing resources indicated by the reasons, the platform continues automatically. The same applies to multi-node units in PD deployments.

For all creation fields, see the [API Reference](../reference/api.md).

## Add Replicas to an Existing Deployment

1. Open **Edit replicas** in deployment details and click **Add a suggested replica**.
2. Check the populated node list and save after confirming it is correct. If no group is recommended, check the number of excluded nodes by category, satisfy the missing conditions indicated by the reasons, then select nodes again.

Nodes in a recommended group must be online, schedulable, satisfy node requirements, and share the same compute network and GPU architecture. Each node must have enough free cards for one rank.
The platform evaluates placement rank by rank using the deployment configuration, then populates the list. A leader does not share a node with another replica's leader.

Recommendations still require your confirmation. The platform does not select nodes automatically for multi-node replicas because crossing switches or unavailable RDMA connectivity can affect the entire replica group.
