# Kunlun P800 Qwen catalog package

This optional package adds Qwen3-0.6B and Qwen3.8-27B text inference profiles to the existing model identities. It does not replace the upstream built-in image matrix or any NVIDIA/Huawei profile. See [中文说明](README.zh-CN.md).

## Import

1. Build the small catalog archive: `python3 build/inference-templates/kunlun-p800/build.py --output /tmp/kunlun-p800-qwen.tgz`.
2. Make the reviewed private-registry image available to the selected Docker nodes, with registry credentials if required. The specs pin its manifest digest directly. `images.json` is an optional legacy tag-based catalog entry; it is not a built-in or public runner image and is not required for these specs.
3. In **Inference templates → Import**, upload `kunlun-p800-qwen.tgz`. Both specs are user profiles named `bf16-vllm-kunlun-p800`; existing profiles are retained.
4. Select the profile under Qwen3-0.6B or Qwen3.8-27B. Use Linux amd64, Docker, whole P800 PCIe/KL3 cards. Start with one card and the package defaults. Qwen3.8 is text-only here.

The template uses the reviewed private-registry K26 vLLM 0.25.1 build, with the Qwen3.8 compatibility changes, recorded as manifest `sha256:b89a290ad64f62c24ca52923f509d6d906c7753b47c9d3dd4690003f58bc1aae`. The actual interpreter is `/opt/vllm025-venv/bin/python`. Registry publication to the upstream runner matrix is a separate step; this package does not invent public mirror references.

Weights remain official, original BF16 files (no AWQ/GPTQ/INT4 replacement). The template requests FP16 computation: `float16`. This is a runtime cast, not quantized replacement weights. Revisions are pinned in each model file. Weight sizes and KV estimates are carried from the corresponding built-in original-weight profiles; they are not new measurements of this image.

GPUWorks single-node device injection verifies and exposes only the selected XPUs and control device. It does not require XDR, RDMA devices, IPC_LOCK or relaxed memlock. The image and local multi-card communication combination still require validation; this does not promise arbitrary image compatibility.

## Single-node TP

Keep nnodes=1 and PP=1; TP is the local card count (1/2/4/8). Cross-node launches are not enabled in this stage. Defaults remain context 2048, one sequence, 256 batched tokens and eager execution.

## Validation boundary

The Go package test validates package/image import, official revisions, card predicates, local visibility, single-node TP, fixed interpreter/dtype and launch refusals. Existing P800 runs establish the underlying Qwen recipes; importing these new package files is not itself a fresh hardware inference measurement. Check the release validation report for the exact code/image that was run.

The general Web evaluation-default inheritance fix must also be present for untouched form defaults to keep the template dtype. It is delivered separately from hardware enablement. Before starting on older servers, explicitly choose Float16 and verify the command preview.

The spec pins the reviewed manifest digest. The optional image catalog entry retains a mutable tag because that import API accepts tags; do not treat it as an immutable or built-in entry.
