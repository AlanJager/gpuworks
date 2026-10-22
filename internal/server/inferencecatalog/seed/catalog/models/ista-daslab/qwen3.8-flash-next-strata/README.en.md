# Qwen3.8-Flash-Next 125B (Strata tiered inference)

[Strata](https://github.com/Niko1221/Strata) keeps the MoE experts tiered across VRAM, RAM and SSD, so a single NVIDIA card with 12 GiB or more runs the 125B Qwen3.8-Flash-Next (IQ2_XS, about 68 GB of weights). The template uses the callable engine: the image entrypoint `/opt/strata/gpuworks-entrypoint.sh` turns the platform's model path, address, port and assigned GPUs into Strata's setup arguments and starts the server.

## Image input

On by default. The image carries Strata's image encoder `strata-vision` and the vision projector `mmproj-Qwen3.8-Flash-Next-BF16.gguf` at the weights' revision (the file sits at the repository root while the template downloads only `IQ2_XS/`, so the image supplies it; a copy of the same name inside the model directory takes precedence). The entrypoint installs with `--vision gpu`: the encoder shares the engine's card, and the engine shrinks its expert cache by what the encoder holds. Measured on one RTX 4090D at a 128K context: the encoder holds about 1.4 GB of VRAM and decoding runs at about 51–55 tok/s, on par with image input off.

- Requests put OpenAI `image_url` parts in a message, as base64 data URLs or http(s) addresses; the GPU encoder turns one image into at most 1024 image tokens.
- Add `--vision cpu` to the custom arguments to encode on the CPU (no VRAM, at most 300 tokens per image, slower), or `--vision no` to turn image input off and leave all VRAM to the expert cache.
- When you turn it off, also set the model's input modalities to text only on the Models page, so Playground stops offering image upload.

## How many cards

Strata uses the cards the platform assigns to the deployment: one when placement picks, or as many as you select in the deploy form, and it splits the layers across them. More cards hold more experts in VRAM and generate faster. Every card needs at least 12 GiB, or the deploy evaluation refuses it.

Neither the card count nor the indexes appear in the template; that is deliberate. The assigned cards reach Strata like this:

1. The scheduler picks the cards. The template's placement resolver only checks each card's memory and pins no count.
2. When the agent starts the container it puts those cards in `CUDA_VISIBLE_DEVICES`, as for every engine. On a udocker node these are the physical indexes; on a docker node the container gets only the selected cards and the variable holds their in-container numbers 0..n-1. Either way they are the numbers `nvidia-smi` shows inside the container.
3. The image entrypoint reads `CUDA_VISIBLE_DEVICES` and passes `--gpu N` for one card or `--gpus A,B` for several. Strata itself picks cards by `nvidia-smi` numbering.

The indexes are not a template variable because they depend on the node's container runtime, which only the agent knows; template variables are expanded on the server, before that is settled. So do not write `--gpu`/`--gpus` into the custom arguments: a fixed index can point at a card another deployment owns.

To see which cards Strata uses, check the `gpu` field of its install config `/opt/strata/strata-iq2_xs.json` in the container (the instance terminal in the deployment details can show it), or run `nvidia-smi` on the node and confirm that only the deployment's cards hold its memory.

## Configurable arguments

A callable profile has no parameter dictionary; you tune it by adding command-line arguments in the deploy form's custom arguments. The entrypoint hands them to Strata's `setup.py` unchanged. A repeated argument takes its last value, so what you add replaces the entrypoint's default.

| Argument | Default | Meaning |
|---|---|---|
| `--context N` | Strata's recommendation for the cards (below) | The context window: the most prompt plus reply tokens of one request |
| `--kv int8\|q4_0\|k8v4` | `int8` | KV precision above an 8K context; `q4_0` halves the KV memory at a small quality cost |
| `--kv-streaming auto\|on\|off` | `auto` | From a 64K context, keep the KV cache in RAM and only the attention window in VRAM |
| `--rope-scaling linear\|yarn`, `--rope-scale F` | yarn past 262144, factor = context / 262144 | RoPE extension past the trained length |
| `--vision gpu\|cpu\|no` | `gpu` | Run the image encoder on the GPU or the CPU, or turn image input off (see Image input) |
| `--vram-reserve-mib N` | engine default | VRAM left free for other programs on the card |
| `--gpu N`, `--gpus A,B` | the cards the platform assigned | Do not change: the entrypoint derives them from `CUDA_VISIBLE_DEVICES` (above), and other cards belong to other deployments |

A request's `max_tokens` caps that reply. Without it Strata generates until the context is full, so one reply is at most the context minus the prompt.

## Context default and limits

- **Default**: without `--context`, Strata recommends by the smallest assigned card: 32K below 14 GB of VRAM, 64K below 20 GB, 128K otherwise. Its RAM rule applies to the IQ3 sizes only, not to IQ2_XS. A 24 GB RTX 4090 / 4090D therefore gets 128K.
- **Maximum**: the model is trained to 262144 (256K). Strata also offers 384K and 512K through yarn extension, marked experimental.
- **Cost**: KV is small, about 13.4 KB per token in int8 (7.3 KB with `q4_0`), so a 256K context needs about 3.5 GB of KV split across the cards by layer. That VRAM is shared with the expert cache, though: every GB more of context is a GB fewer of cached experts, and generation slows down accordingly. On small cards keep the default, and pair long contexts with `--kv q4_0` or `--kv-streaming on`.
- The model's `context_length: 262144` is the trained length, not the context a deployment serves. The served value is in Strata's start log (`context N tokens`) and in `/v1/models` (`meta.n_ctx`).

## Known limits

- No `/metrics`: the platform collects no engine metrics.
- The first start builds the expert index and is slower than a restart.
