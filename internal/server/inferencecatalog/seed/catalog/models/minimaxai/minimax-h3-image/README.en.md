# MiniMax-H3-Image

MiniMax-H3-Image generates and edits images on the MiniMax-H3 DiT: six CFG-distilled steps per image, one image per request, and up to nine reference images for an edit. The GPUWorks Playground runs text-to-image and single-image edits directly, and both `/v1/images/generations` and `/v1/images/edits` are served through the platform gateway.

## Weights and components

The template has two components. The platform downloads and mounts both at deploy time:

| Component | Source | Size | Container path |
|---|---|---:|---|
| Primary (DiT, VAE, PDD output heads, tokenizer) | `MiniMaxAI/MiniMax-H3-Image` | 68.7 GB | `/models/MiniMax-H3-Image` |
| Qwen3-VL text encoder | the `text_encoder` folder of `MiniMaxAI/MiniMax-H3` | 66.7 GB | `/models/MiniMax-H3` |

The upstream `model_index.json` loads the MiniMax-H3 text encoder from the Hub by repository name. Before the engine starts, the template points it at the mounted folder, so the engine stays offline (`HF_HUB_OFFLINE=1`). Only the `text_encoder` folder is downloaded; a node that already caches the complete MiniMax-H3 repository at the same revision reuses it.

Hugging Face revisions after 2026-11-10 also carry a diffusers-format copy of the weights (about 204 GB for the whole repository) that vLLM-Omni does not use, so the template pins the revision before it.

## Profiles

The image is the upstream release image `vllm/vllm-omni:minimax-h3-image` (vLLM 0.33.0 with vLLM-Omni), and the arguments follow the [vLLM recipe](https://recipes.vllm.ai/MiniMaxAI/MiniMax-H3-Image). All five profiles were run on H20-3e (141 GB per GPU); the figures are end-to-end medians at concurrency 1:

| Profile | GPUs | Layout | Peak per GPU | 2048² text-to-image | 2048² edit |
|---|---:|---|---:|---:|---:|
| 4 GPUs Ulysses | 4 | Ulysses SP4, text encoder TP4, all resident | 83.0 GiB | 2.55 s | 7.8 s |
| 4 GPUs TP | 4 | DiT TP4, all resident | 35.5 GiB | 2.63 s | 7.8 s |
| 2 GPUs Ulysses | 2 | Ulysses SP2, all resident | 94.3 GiB | 4.70 s | 14.6 s |
| 1 GPU · text encoder offload | 1 | DiT resident, text encoder layerwise offload | 71.4 GiB | 9.55 s | 27.3 s |
| 1 GPU · layerwise offload | 1 | DiT and text encoder layerwise offload | 12.7 GiB | 9.53 s | 27.3 s |

- Requests below 2048² are generated on a canvas of at least 2048² and resized back, so 1024² and 2048² take about the same time. Requesting a canvas size directly (2048x2048, 2720x1536, 1536x2720, 2368x1760, 2496x1664, 3136x1344) skips the resize.
- The engine serves one request at a time: concurrency only adds queueing, and throughput does not rise with it.
- The two offload profiles keep weights in pinned host memory (about 62 GiB for the text encoder, plus about 62 GiB for the DiT with layerwise offload); the template lifts the `memlock` limit for them.
- On 80 GB GPUs (H100) SP4 does not fit: use 4 GPUs TP or 1 GPU layerwise offload. Where one GPU holds everything (B200, B300), full single-GPU residency is the upstream recommendation; the template does not offer it yet.

## Request contract

- `num_inference_steps` must be omitted or 6, `guidance_scale` omitted or 1, `negative_prompt` is not supported, and `n` is 1.
- References are presented to the model as `Picture 1`, `Picture 2`, … in upload order, and prompts can name them. Giving references at the canvas size of the requested aspect ratio and requesting that size aligns edits best.
