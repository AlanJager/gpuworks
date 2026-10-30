# Kunlun P800 Qwen catalog package

This optional package adds Qwen3-0.6B and Qwen3.8-27B text inference profiles to the existing model identities. It does not replace the upstream built-in image matrix or any NVIDIA/Huawei profile. See [中文说明](README.zh-CN.md).

## Import

1. Build the small catalog archive: `python3 build/inference-templates/kunlun-p800/build.py --output /tmp/kunlun-p800-qwen.tgz`.
2. Make the reviewed private-registry image available to the selected Docker nodes, with registry credentials if required. The specs pin its manifest digest directly. `images.json` is an optional legacy tag-based catalog entry; it is not a built-in or public runner image and is not required for these specs.
3. In **Inference templates → Import**, upload `kunlun-p800-qwen.tgz`. Both specs are user profiles named `bf16-vllm-kunlun-p800`; existing profiles are retained.
4. Select the profile under Qwen3-0.6B or Qwen3.8-27B. Use Linux amd64, Docker, whole P800 PCIe/KL3 cards. Start with one card and the package defaults. Qwen3.8 is text-only here.

The template uses the reviewed private-registry K26 vLLM 0.25.1 build, with Qwen3.8 compatibility and PP/DP worker-rank/device-mapping fixes, recorded as manifest `sha256:33d94768ff9a895bdc8874cae09940aede1b0e88218e52ed52231b8f86a91beb`. The actual interpreter is `/opt/vllm025-venv/bin/python`. Registry publication to the upstream runner matrix is a separate step; this package does not invent public mirror references.

Weights remain official, original BF16 files (no AWQ/GPTQ/INT4 replacement). The template requests FP16 computation: `float16`. This is a runtime cast, not quantized replacement weights. Revisions are pinned in each model file. Weight sizes and KV estimates are carried from the corresponding built-in original-weight profiles; they are not new measurements of this image.

GPUWorks single-node device injection verifies and exposes only the selected XPUs and control device. It does not require XDR, RDMA devices, IPC_LOCK or relaxed memlock. The image and local multi-card communication combination still require validation; this does not promise arbitrary image compatibility.

## Parallel deployment

- TP means Tensor Parallelism; PP means Pipeline Parallelism; engine DP means Data Parallelism. For TP-only deployment, set PP=1 and DP=1. TP×PP×DP determines the total GPU requirement, while GPU count and CUDA-visible indexes in this package are per node. Two nodes × four cards with PP=DP=1 means TP8.
- Use the locked PP/DP image, not the earlier RDMA-only image: that image lacks the rank-sharded device mapping needed by DP. Engine DP also requires the server allocation fix for all replicas. Platform replicas are a separate deployment mechanism.
- Cross-card communication must work on the host before TP/PP can succeed; an image update cannot correct a host XRE/OAM identity conflict. The following network requirements apply only across nodes.
- Select a verified compute network. The launcher copies its system `GLOO_SOCKET_IFNAME` to BKCL; do not put an arbitrary management NIC in the template.
- Set the deployment environment `BKCL_RDMA_NICS` to the verified RDMA **netdev** list for the selected cards on each node, for example `bond0,bond0` for two local cards when that bond is the actual RoCE netdev. This is not an HCA name such as `mlx5_0:1`. The package intentionally cannot guess a site's bridge/bond mapping; it refuses a multi-node launch without this setting. Nodes with different mappings need appropriate per-node deployment configuration before use.
- Hosts need compatible native/XDR drivers and functional RDMA. The resolved host-RDMA network provides verbs devices, unlimited memlock and IPC_LOCK; the prober adds selected XPU devices and XDR only. Multi-node launches without these resolved resources, including finite memlock overrides, are refused. The template does not pass extra GPU devices, hardcode physical indexes or load kernel modules.
- Confirm the selected cards form a working collective group and are free. Zero memory usage alone is not proof of availability. Reuse the existing Docker daemon; do not start another daemon or change docker0.

Defaults are conservative: context 2048, one sequence, 256 batched tokens, 70% memory, eager execution. They are a bring-up configuration, not a throughput recommendation. Supported local cardinalities are 1/2/4/8; model/global-TP divisibility and runtime constraints still apply.

## Validation boundary

The Go package test validates package/image import, official revisions, card predicates, local visibility, global TP, fixed interpreter/dtype and launch refusals. Existing P800 runs establish the underlying Qwen recipes; importing these new package files is not itself a fresh hardware inference measurement. Check the release validation report for the exact code/image that was run.

The general Web evaluation-default inheritance fix must also be present for untouched form defaults to keep the template dtype. It is delivered separately from hardware enablement. Before starting on older servers, explicitly choose Float16 and verify the command preview.

The spec pins the reviewed manifest digest. The optional image catalog entry retains a mutable tag because that import API accepts tags; do not treat it as an immutable or built-in entry.

The multi-node image additionally includes Ubuntu RDMA userspace libraries (`libibverbs`, `librdmacm`, `libmlx5`) and providers. Passing GPU/XDR devices alone does not supply these libraries. Single-node device injection remains independent of XDR.
