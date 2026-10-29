# Kunlun P800 Deployment

## Current Capabilities

P800 PCIe / KL3 uses vendor `kunlunxin` and framework `xre`. On Linux amd64, only Docker runtime has been validated; a runtime requirement or adapter listing does not establish nerdctl support. This combination supports whole-card single-GPU inference, single-node TP (Tensor Parallelism). Cross-node TP is not enabled in this stage. PP, engine DP, PD, partitions and sleep are unavailable. Independent single-node replicas are not cross-node TP.

Reuse the host’s existing Docker daemon. Independent sockets/data roots do not isolate host networking. Do not launch another dockerd with `--bridge=none` in the same network namespace: it can remove the existing `docker0`. Image and inference testing does not require restarting Docker; offline probes use container-level `--network none`.

Native discovery uses `xpu-smi -q -x`. It repairs only the observed closing-tag typo within process_info; other malformed XML fails. Unknown ECC produces a telemetry warning without rejecting scheduling. Known uncorrectable ECC errors make that card unschedulable; peers remain reported. PCI inventory supplies cards omitted by the CLI. Inventory reads may use the command cache, but device injection freshly verifies UUID, PCI BDF, minor and sysfs character devices. The common compute-process inventory continuously reports occupancy.

GPUWorks single-node device injection verifies and exposes only the selected XPUs and control device. It does not require XDR, RDMA devices, IPC_LOCK or relaxed memlock. The image and local multi-card communication combination still require validation; this does not promise arbitrary image compatibility.

## Images and Model Profiles

Hardware support does not automatically supply an inference profile for every model. The repository has no default published Kunlun runner matrix entry; use the existing image and template import APIs to supply verified images and user specs.

Qwen3-0.6B and Qwen3.8-27B are available as an [importable template package](../../build/inference-templates/kunlun-p800/README.md). It adds only user profiles. The package specs pin the reviewed private-registry manifest digest. The optional tag image-catalog entry is neither a built-in runner nor a public release.

1. Prepare the verified image on the selected node. Record immutable digests, engine version and the actual Python/entrypoint paths. CUDA-compatible fields do not establish native runtime support.
2. Register local weights with their full repository identity, such as `deepseek-ai/DeepSeek-R1-Distill-Qwen-7B`. A short directory name is not a repository identity. Existing files can be registered correctly without copying or downloading them.
3. A spec’s `weights.sources[].repo_id` must match the model’s `source_id`; set `gpus.vendor` to `kunlunxin` and match the `P800 PCIe` card name. Missing specs or mismatched identities leave the Web form without a compatible profile.
4. Use the image’s verified native entrypoint and explicit dtype, context and memory settings. Images that depend on device enumeration may use `env -u XPU_VISIBLE_DEVICES`; do not mistake host indexes for container-logical indexes.
5. Use TP=1 for one card, or TP equal to the selected local card count. Keep PP=1 and nnodes=1.

## Validation and Boundaries

Verify one card first, then the selected local card group. In Web, choose the correct profile, one node, TP equal to the selected card count and PP=1. Check model/image sources, health, ordinary HTTP, SSE DONE and visible response text. A handshake or HTTP 200 alone does not prove a valid text response. Cross-node requests currently produce an unsupported placement refusal.

See the [vendor support matrix](../reference/support-matrix.md) for related boundaries.

The hardware catalog displays `Kunlunxin P800 PCIe` and stores vendor `kunlunxin`; `NormalizeGPUName` removes the brand before spec matching, so the reviewed `^P800 PCIe$` predicate still matches. Hardware identity feeds capability selection as runtime and model combinations are validated. Partition availability comes from the dynamic partition capability check; there is no static Partition feature axis. The engine Profile still defines engine PD modes. Vendor restrictions are defined only in the accelerator capability table; `ProfileForTarget` derives the modes available on that target from it.
