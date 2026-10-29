# Kunlun P800 Deployment

## Current Capabilities

P800 PCIe / KL3 uses vendor `kunlunxin` and framework `xre`. On Linux amd64, only Docker runtime has been validated; a runtime requirement or adapter listing does not establish nerdctl support. This combination supports whole-card single-GPU inference, single-node TP (Tensor Parallelism), and conditional cross-node TP. PP, engine DP, PD, partitions and sleep are unavailable. Independent single-node replicas are not cross-node TP.

Reuse the host’s existing Docker daemon. Independent sockets/data roots do not isolate host networking. Do not launch another dockerd with `--bridge=none` in the same network namespace: it can remove the existing `docker0`. Image and inference testing does not require restarting Docker; offline probes use container-level `--network none`.

Native discovery uses `xpu-smi -q -x`. It repairs only the observed closing-tag typo within process_info; other malformed XML fails. Unknown ECC produces a telemetry warning without rejecting scheduling. Known uncorrectable ECC errors make that card unschedulable; peers remain reported. PCI inventory supplies cards omitted by the CLI. Inventory reads may use the command cache, but device injection freshly verifies UUID, PCI BDF, minor and sysfs character devices. The common compute-process inventory continuously reports occupancy.

GPUWorks single-node device injection verifies and exposes only the selected XPUs and control device. It does not require XDR, RDMA devices, IPC_LOCK or relaxed memlock. The image and local multi-card communication combination still require validation; this does not promise arbitrary image compatibility.

## Images and Model Profiles

Hardware support does not automatically supply an inference profile for every model. The repository has no default published Kunlun runner matrix entry; use the existing image and template import APIs to supply verified images and user specs.

Qwen3-0.6B and Qwen3.8-27B are available as an [importable template package](../../build/inference-templates/kunlun-p800/README.md). It adds only user profiles. The package specs pin the reviewed private-registry manifest digest. Configure native communication as described in the package for two-node launches. The optional tag image-catalog entry is neither a built-in runner nor a public release.

1. Prepare the same verified image on both nodes. Record immutable digests, engine version and the actual Python/entrypoint paths. CUDA-compatible fields do not establish native runtime support.
2. Register local weights with their full repository identity, such as `deepseek-ai/DeepSeek-R1-Distill-Qwen-7B`. A short directory name is not a repository identity. Existing files can be registered correctly without copying or downloading them.
3. A spec’s `weights.sources[].repo_id` must match the model’s `source_id`; set `gpus.vendor` to `kunlunxin` and match the `P800 PCIe` card name. Missing specs or mismatched identities leave the Web form without a compatible profile.
4. Use the image’s verified native entrypoint and explicit dtype, context and memory settings. Images that depend on device enumeration may use `env -u XPU_VISIBLE_DEVICES`; do not mistake host indexes for container-logical indexes.
5. A single-card spec may fix TP=1. A cross-node placement resolver checks local card count but leaves global TP to the deployment: two nodes with two cards each use TP=4, PP=1, nnodes=2, not TP=2 from the resolver.

## Two-Node Communication Prerequisites

- Both nodes need working XDR compatible with their kernels. The agent only checks and exposes devices; it does not install, compile or load drivers, or alter KASLR/boot entries.
- Configure a reachable compute network and verify the inventory’s mapping among RDMA ports, interfaces and IPs. Inventory neither changes networking nor establishes measured bandwidth.
- The common network layer supplies `/dev/infiniband`, unlimited memlock and `IPC_LOCK` for a host RDMA compute network. The Kunlun prober injects only the selected XPUs, control device and XDR; it does not enumerate all HCAs or duplicate network arguments. XDR must be a real character device. Multi-node launches explicitly refuse missing resolved resources or finite memlock limits.
- `IPC_LOCK` and memlock relaxation serve communication-memory registration. Templates and host mounts use the common administrator trust model; do not add other GPUs or device directories.

Images must supply the corresponding RDMA userspace libraries and verified communication parameters. The platform does not introduce global defaults that bypass driver checks or modify kernel boot arguments. Check the supplier’s supported kernel, driver and image combinations before use.

## Validation and Boundaries

Verify each node on one card first, then collective communication and two-node deployment. In Web, choose the correct profile, two nodes and TP=4/PP=1; check each rank’s command and model/image acquisition source, then health, ordinary HTTP, SSE DONE and visible response text. A handshake or HTTP 200 alone does not prove a valid text response.

See the [vendor support matrix](../reference/support-matrix.md) for related boundaries.

The hardware catalog displays `Kunlunxin P800 PCIe` and stores vendor `kunlunxin`; `NormalizeGPUName` removes the brand before spec matching, so the reviewed `^P800 PCIe$` predicate still matches. Hardware identity feeds capability selection as runtime and model combinations are validated. Partition availability comes from the dynamic partition capability check; there is no static Partition feature axis. The engine Profile still defines engine PD modes. Vendor restrictions are defined only in the accelerator capability table; `ProfileForTarget` derives the modes available on that target from it.
