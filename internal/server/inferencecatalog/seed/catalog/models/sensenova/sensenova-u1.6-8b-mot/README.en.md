# SenseNova-U1.6-8B-MoT

SenseNova-U1.6-8B-MoT is SenseTime's NEO-unify text-to-image and image-editing model (a Qwen3-8B backbone plus a generation tower and pixel head, `neo_chat` model type): 35.07 GB of BF16 weights. The GPUWorks Playground runs text-to-image and single-image edits directly, and both `/v1/images/generations` and `/v1/images/edits` are served through the platform gateway.

## Weights

The two profiles use different weight repositories:

- **BF16 profile**: the original weights (35.07 GB, `weight_bytes` 35065860072):
  - Hugging Face `sensenova/SenseNova-U1.6-8B-MoT`, revision `8f582699bac7f1e5362df2a776397d0e1a963b6c`.
  - ModelScope `SenseNova/SenseNova-U1.6-8B-MoT`, revision `c03718b305258bbdc3994e132158ddfbda842268`.
  - Both sources match by sha256 (eight safetensors shards plus tokenizer/config).
- **FP8 profile**: a pre-quantized FP8 W8A8 (per-tensor) checkpoint (18.86 GB, `weight_bytes` 18864507420), quantized offline from the original BF16 weights above (derived from original revision `8f58269`):
  - Hugging Face `gpuworks/SenseNova-U1.6-8B-MoT-FP8`, revision `380e219037baa980ce598fc1084b9a58c5fb1294`.
  - ModelScope `gpuworks/SenseNova-U1.6-8B-MoT-FP8`, revision `80dc710238d2d7198f896cea4e17409534cf7646`.
  - The repository's `config.json` carries `quantization_config`, which vLLM-Omni detects on its own, so the launch needs no `--quantization fp8`.

## Image and engine

Both profiles run GPUWorks' **vLLM-Omni 0.34** image (`gpuworks/gpuworks-runner:cuda13.0-vllm-omni0.34.0-r1`, vLLM 0.34.0 plus vLLM-Omni 0.34.0) with `--omni`. Neither declares an offload profile: vLLM-Omni reports `No _layerwise_offload_blocks_attrs defined` for the SenseNova architecture (`SenseNovaU1ForCausalLM`) (measured on 0.32; 0.34 has no support either), which is not in upstream's layerwise-offload support table.

## Profiles

| Profile | GPUs | Fits | Peak per GPU |
|---|---:|---|---:|
| FP8 · 1 GPU | 1 | Ampere+, ≥ 22 GiB | 21.1–22.0 GiB |
| BF16 · 1-2 GPUs | 1 or 2 | ≥ 40 GiB on 1, or ≥ 22 GiB per GPU on 2 | 36.0 GiB on 1; 17–18.7 GiB/GPU on 2 |

**FP8** (`--omni`, with the pre-quantized FP8 checkpoint): Ada and above use CUTLASS W8A8; Ampere (SM86, e.g. RTX 3090) has no FP8 tensor cores and falls back to Marlin W8A16 (FP8 weights, BF16/FP16 activations). Both paths are verified on real hardware with this template's image and this checkpoint (RTX 4090, RTX 3090). The pre-quantized checkpoint produces pixel-identical output to vLLM-Omni's online FP8 (`--quantization fp8` on the BF16 original); the difference is only in loading: 18.9 GB on disk and to download (35.1 GB for the original), and about 2.8 GiB peak host memory while loading (about 12.3 GiB when quantizing the BF16 original online; both measured on a vast 4090).

**BF16** (`--omni`, no quantization) needs the full 35 GB checkpoint resident on one GPU; with two GPUs the platform injects `--tensor-parallel-size` from the placed GPU count and the model shards across them with tensor parallelism. When both layouts fit, one GPU is preferred.

## Measured performance

Each figure is an end-to-end median (including network and gateway overhead unless marked "engine direct") at 50 steps, concurrency 1. The "Image / engine" column says which image and vLLM-Omni release served the request: **this template's image r1** is `cuda13.0-vllm-omni0.34.0-r1`; the other rows are earlier measurements on the upstream `vllm/vllm-omni:v0.34.0` image or the old 0.32.0 image, kept for comparison.

| Profile | Card and source | Image / engine | 1024×1024 | 2048×2048 | Peak per GPU |
|---|---|---|---:|---:|---:|
| FP8 · 1 GPU | RTX 4090 24GB (vast rental, engine direct, pre-quantized FP8) | this template's image r1 / 0.34.0 | 8.3 s | 37.2 s | 21.1 GiB |
| FP8 · 1 GPU | RTX 4090 24GB (same machine, engine direct, online FP8 of the BF16 original) | this template's image r1 / 0.34.0 | 8.3 s | 37.3 s | 21.7 GiB |
| FP8 · 1 GPU | RTX 4090 24GB (vast rental, a different machine, online FP8) | upstream vllm-omni:v0.34.0 / 0.34.0 | 9.2–10.2 s | 42.0 s | 21.68 GiB |
| FP8 · 1 GPU | RTX 4090 48GB (this template's old image, online FP8) | old 0.32.0-r2 / 0.32.0 | 8.7–9.1 s | 38.1 s | 21.74 GiB |
| FP8 · 1 GPU | RTX 3090 24GB (vast rental, engine direct, pre-quantized FP8) | this template's image r1 / 0.34.0 | 27.5 s | 119.4 s | 21.1 GiB |
| FP8 · 1 GPU | RTX 3090 24GB (same machine, engine direct, online FP8 of the BF16 original) | this template's image r1 / 0.34.0 | 27.5 s | 119.4 s | 22.1 GiB |
| FP8 · 1 GPU | RTX 3090 24GB (vast rental, a different machine, online FP8) | upstream vllm-omni:v0.34.0 / 0.34.0 | 27.4–30.0 s | 120.3 s | 21.99 GiB |
| BF16 · 1 GPU | RTX 4090 48GB (this template's old image) | old 0.32.0-r2 / 0.32.0 | 12.5–13.3 s | 55.0 s | 36.04 GiB |
| BF16 · 2 GPUs TP2 | 2 x RTX 3090 24GB (old image, deployed through the GPUWorks platform, measured through the gateway) | old 0.32.0-r2 / 0.32.0 | 24.9 s | 100.5 s | 18.66 GiB/GPU |
| BF16 · 2 GPUs TP2 | 2 x RTX 3090 24GB (docker, a separate run) | upstream vllm-omni:v0.34.0 / 0.34.0 | 21.4 s | 86.8 s | 17.1–18.2 GiB/GPU |
| BF16 · 2 GPUs TP2 | 2 x RTX 3090 24GB (runveil, cross-topology, no NVLink) | upstream vllm-omni:v0.34.0 / 0.34.0 | 25.6 s | 124.9 s | 17.1–18.2 GiB/GPU |

Notes:

- Only 24-48 GB consumer cards (RTX 3090/4090) were measured. Both single-GPU FP8 paths (Ada's CUTLASS, Ampere's Marlin) are verified on real hardware with this template's image r1; there is no data yet for a larger-memory card (A100/H100/H20).
- The two BF16 profiles were not re-measured with image r1 (the rented 4090 has only 24 GB, and no ≥ 40 GB single card or two-GPU machine was available); the BF16 rows come from the old or the upstream image and are for reference. r1 runs the same engine line (vLLM-Omni 0.32 → 0.34).
- Old and new images differ little on a single-GPU FP8 run of the same card model: r1 takes 8.3 s at 1024×1024 on a 4090, the old 0.32 image 8.7–9.1 s on a 48GB 4090, and the upstream 0.34 image 9.2–10.2 s on another 24GB rental (host differences are part of those gaps and cannot all be credited to the image).
- The RTX 3090 (SM86, Ampere) has no FP8 hardware support, so FP8 there is more than 3x slower than on a 4090 (Ada, native W8A8) due to Marlin's weight-dequantization overhead. Where faster FP8 matters more than memory, use an Ada/Hopper card or BF16 on a larger card instead.
- NVLink between the two TP2 GPUs matters: on the same pair of 3090s, an NVLink pair (the docker example) is 1.18-1.44x faster than a cross-PCIe pair with no NVLink (the runveil example), with the gap wider at 2048x2048.
- Cold start (deployment created to health check passing): with weights already on shared storage, TP2 BF16 measured about 82 seconds (including an image pull; weights needed no download). A first-time weight download adds weight size divided by download bandwidth on top.
- Edit (`/v1/images/edits`, a 1024x1024 reference image): with image r1, 9.4 s (pre-quantized) / 9.7 s (online FP8) on a 24GB 4090 and 29.9 s (pre-quantized) on a 24GB 3090; 26.7 s on TP2 BF16 with the old image.
- The 1344x768 and 768x1344 aspect ratios were verified against a real TP2 BF16 deployment of the old (0.32) image (24.6-24.8 s each, close to the square profile at the same pixel count); the returned image's pixel size matches the request.

## Known limits

- **No offload profile**: neither vLLM-Omni 0.32 nor 0.34 supports layerwise VRAM offload for this model, so there is no lower-memory route than FP8; cards under 24 GB cannot run it.
- **FP8 is slower on Ampere**: the RTX 3090's Marlin W8A16 path takes close to 2 minutes per image at 2048x2048. It is useful when memory is tight and speed is not, but faster FP8 needs an Ada/Hopper card, or BF16 on a larger card.
- **FP8 on Ampere needs a recent server**: the dtype admission matrix before 2026-11-24 refused FP8 on Ampere (`dtype_unsupported_detail`); a server needs the fix from that date to accept this profile on an RTX 3090-class card. Ada and above are unaffected.
- **The first request is slower**: the engine compiles and warms up graphs on its first inference after startup, a one-time cost not included in the medians above.

## Calling the API

Text-to-image:

```bash
curl -X POST "$GPUWORKS_URL/v1/images/generations" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -H "Content-Type: application/json" \
  --data '{"model":"<served-model>","prompt":"A sunrise over mountains","size":"1024x1024"}'
```

Image edit (`image` is the source file):

```bash
curl -X POST "$GPUWORKS_URL/v1/images/edits" \
  -H "Authorization: Bearer $GPUWORKS_API_KEY" \
  -F image="@input.png" \
  -F model=<served-model> \
  -F prompt="Make the background blue"
```

`size` offers three square profiles (1024x1024, 1536x1536, 2048x2048) and two near-16:9 aspect ratios (1344x768, 768x1344), all multiples of 32 and verified against a real deployment.
