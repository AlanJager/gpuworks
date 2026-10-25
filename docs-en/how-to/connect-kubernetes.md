# Connect a Kubernetes Cluster

**Use case**: You already have a Kubernetes cluster with GPUs and want to hand some of its GPU nodes to gpuworks for model deployment without installing an agent on each machine.

For step-by-step instructions for managed public cloud clusters (Alibaba Cloud ACK, DigitalOcean DOKS, Nebius mk8s), see [Connect Managed Kubernetes from a Public Cloud](connect-managed-kubernetes.md).

**Prerequisites**:

- The server is running, and the cluster nodes can reach the server address (the agent connects outbound; no inbound node port is needed).
- The cluster uses containerd (kubeadm, k3s, RKE2, microk8s, and GPU Operator environments all do). CRI-O (OpenShift) and Docker nodes are not supported; the agent refuses to start and states the reason.
- One cluster entry corresponds to nodes of one vendor: NVIDIA, Ascend, Hygon, or PPU. On NVIDIA nodes, the NVIDIA container runtime must be available: either as containerd's default runtime (as on managed Kubernetes such as DOKS, Nebius, and ACK), or through a RuntimeClass named `nvidia` (k3s creates it automatically). Other vendors only require the vendor driver on the node; they need neither the vendor container runtime nor the vendor device plugin.
- Nodes that mount NFS storage have `mount.nfs` installed (nfs-utils / nfs-common): the agent uses it in the node's mount namespace.
- You can run `kubectl apply` in the cluster, and a `privileged` namespace is allowed (the manifest labels the namespace with the Pod Security Admission `privileged` label; policies such as Kyverno or Gatekeeper must be relaxed separately).

## How It Works

A cluster joins as one DaemonSet. One agent runs on each selected node and exchanges the cluster's connect token for its own node token; from then on it is exactly like an ordinary node: scheduling, deployment, gateway, logs, and terminals make no distinction. An instance becomes a Pod pinned to that node (`restartPolicy: Never`, host network, model directories mounted by hostPath at the same path), and `kubectl get pods -n gpuworks-system` shows every instance. Retries and rebuilds remain gpuworks's responsibility; Kubernetes does not restart failed instances.

Each card has only one allocator at a time. When a device plugin (`nvidia.com/gpu` and similar) is installed on the node, the cards belong to the kubelet: for each card gpuworks wants to use, it starts a placeholder Pod (`gpuworks-hold-*`) that requests only one card. After all instances have stopped and the cluster's “Idle return” time has elapsed, the placeholder Pods are deleted and the cards go back to Kubernetes. Cards held by other Pods appear in gpuworks as externally occupied and are not scheduled. When the node has no device plugin, gpuworks uses the node's cards directly. When the device plugin allocates less than a whole card (time-slicing / MPS reporting one card as several shares, MIG, HAMi, Ascend vNPU, and other split resources), one share is not one card, so gpuworks does not borrow cards: all of the node's cards appear as externally occupied, the reason is stated in the card's occupancy information, and no instance is placed on them; they recover automatically once the cluster returns to whole-card allocation.

The device nodes an instance needs (Ascend `/dev/davinci*`, Hygon `/dev/kfd` and `/dev/dri/*`, PPU `/dev/alixpu*`, and so on) are handed to the instance by the agent's own device plugin: one extended resource per card, `gpuworks.ai/accelerator-<card number>`, and `gpuworks.ai/accelerator-shared` for the control nodes shared by all cards. The instance Pod requests the cards it uses and the shared resource, and the kubelet gives the container only those device nodes, so the instance needs no privileged mode. Which device nodes belong to which card comes from the vendor's own device injection, consistent with docker / nerdctl instances on ordinary nodes. Instances that need devices beyond the plugin (for example InfiniBand `/dev/infiniband`) run privileged.

The agent uses the node's own DNS (`dnsPolicy: Default`), like an agent installed directly on the host, and does not depend on cluster DNS; the server address must therefore be one the node can resolve, not an in-cluster Service name. The agent runs nerdctl (pulling and importing images, terminals) and `mount` (NFS) in the node's mount namespace, so the containerd socket and registry configuration (the `certs.d` generated from k3s's `registries.yaml`) are used at the node's own locations and need no configuration. When Docker's bundled containerd also runs on the node, the agent identifies the one the kubelet uses from its own container's shim.

## Connect

1. On the “Kubernetes” page, click “Connect cluster”, fill in the name, the server address (the address cluster nodes use to reach the server), and the vendor, and optionally the node selector (for example `nvidia.com/gpu.present=true`; empty means all nodes). When one Kubernetes cluster has nodes from two vendors, create two entries, one per vendor, each with a different namespace and node selector.
2. Options under “Advanced”:
   - State directory: the agent's directory on the node, `/opt/gpuworks` by default. When a gpuworks agent is already installed on the node, use a different directory (for example `/opt/gpuworks-kube`) so the two do not interfere.
   - containerd socket: when empty, the agent finds it itself (the default locations of containerd, k3s, RKE2, and microk8s); fill it in if it is installed elsewhere.
   - API server address: only for clusters where nothing on the node provides Service addresses (no kube-proxy or replacement, such as KubeEdge or OpenYurt edge nodes). Write it as `https://host:port`; the address must be in the API server's certificate. When empty, the agent reaches the API server through the cluster's `kubernetes` Service.
   - Image pull secret: when the agent image is in a registry that needs a login (an enterprise Harbor or a cloud provider's private registry), first create a docker-registry Secret in the namespace (`kubectl -n gpuworks-system create secret docker-registry regcred …`; create the namespace first if it does not exist), then enter its name here. The agent Pod uses it to pull the image, and the placeholder Pods the agent starts reuse it.
   - RuntimeClass: NVIDIA only: the RuntimeClass instance Pods use to reach the GPUs. When empty (the default), the agent on each node decides: it uses the cluster's `nvidia` RuntimeClass if there is one, otherwise the node's default runtime; when `nvidia-smi` is not present in the agent's own container, it runs it on the node. Fill it in only when the RuntimeClass has a different name.
   - Engine port range: `40000-42767` by default. Do not overlap the cluster's NodePort range (`30000-32767` by default): kube-proxy diverts NodePort traffic sent to local addresses.
   - Idle return: how long a borrowed card is kept after the last instance stops, 60 seconds by default (checked every 30 seconds, so the actual return happens between 60 and 90 seconds); “Keep borrowed cards” means never return them, handing the whole node pool to gpuworks.
3. Copy the generated command and run it in the cluster:

   ```bash
   kubectl apply -f 'https://server.example/kube/manifest.yaml?token=…'
   ```

   The selected nodes appear on the Nodes page automatically, tagged “K8s · cluster/node name”; nodes added later by scale-out join automatically as well. If no node has joined 5 minutes after creation, “Troubleshoot” in the list gives the `kubectl` commands to run and common causes (the agent image cannot be pulled, the RuntimeClass does not exist, the node selector selects no node, the agent cannot reach the server).

The command contains the cluster's connect token and is shown only to administrators. After the connect token is revoked, new nodes can no longer join, and nodes already joined are unaffected.

## Day-to-Day Operations

| Scenario | What to do |
|---|---|
| Maintain a node | Run `kubectl cordon` in Kubernetes: at the next status report (within a minute) gpuworks likewise stops placing new replicas on that node, and `uncordon` restores it in the same way |
| Upgrade the agent | Click “Upgrade Agent” in the node details: this switches the cluster's agent DaemonSet to the image of the same version as the server, Kubernetes replaces the agents on the nodes in turn, and instance Pods are unaffected. You can also run `kubectl -n gpuworks-system set image ds/gpuworks-agent agent=<image>` yourself |
| A node is deleted or scaled in | After a node has been offline for 15 minutes, gpuworks asks another online node of the same cluster to confirm; when the node no longer exists in Kubernetes and has no instances, it is removed automatically |
| Delete a node from gpuworks | Only the record is deleted and the agent is not uninstalled; if the DaemonSet still selects the node, it rejoins, so adjust the node selector first |
| Uninstall | Run `kubectl delete -f` on the same manifest address: the DaemonSet, instance Pods, and placeholder Pods are all deleted, and the cards go back to Kubernetes |

## Limitations

- Only containerd is supported: images are pulled and imported by the agent through the node's containerd (the kubelet's `k8s.io` namespace), not through the kubelet, so instance images do not use the Pod's `imagePullSecrets`; credentials for private registries are configured in gpuworks's image sources (the cluster entry's image pull secret covers only the agent image).
- Instances use host network and create no Service / Ingress; they are accessed through the gpuworks gateway.
- Vendor command-line tools (`npu-smi`, `hy-smi`, `ppu-smi`) run in the node's mount namespace and use the node's own C libraries and driver libraries, so the agent image is independent of the node's distribution.
- The agent runs privileged with hostNetwork and hostPID (required for probing and for running nerdctl and mount through the node's mount namespace).
