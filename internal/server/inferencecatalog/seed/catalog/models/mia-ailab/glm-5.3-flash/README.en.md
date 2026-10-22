# GLM-5.3-Flash (TensorFold EXL3)

Two measured recipes, both hosting TensorFold on the `callable` engine. The template's adapter turns GPUWorks launch facts (model path, port, parallelism, rank) into TensorFold's process layout:

- **DGX Spark (GB10) x2**: two-node TP2, one rank per node over RoCE; q4 dense, FP8 KV, DFlash2, 1M context, four streams, image/video input. The deterministic quality probes passed, including a hidden needle at roughly 691K input tokens; long-context throughput is prefill-bound.
- **RTX PRO 6000 / 6000D x4 (one PCIe host)**: TP4 on four cards of one host, four ranks in one container; NCCL over PCIe P2P and CUDA-IPC copy engines for bulk exchanges. Reproduces the Aevonix production settings: FP8 dense, FP8 KV, DFlash2 drafts, 40 streams, 1M context, image/video input. The KV pool is capped at 60 GiB per card; on the 84 GB 6000D the engine shrinks it to what fits.

Both profiles serve alike: thinking on by default at reasoning effort `medium`, with image and video input (the checkpoints carry the vision tower, which `--vision` enables; requests send images and clips as data URLs). GLM-5.3's chat template has only Low, High and Max; TensorFold maps `medium` to High, which barely thinks on easy questions and reasons at length on hard ones.

**The first start is slow**: TensorFold compiles its CUDA kernels on first start, and the ranks take turns converting weights on the CPU before loading them onto the cards. On a udocker-only node (AutoDL, 4x RTX 6000D) the first start measured 61 minutes, mostly spent in udocker's system-call forwarding; a docker node is much faster. Set the deploy form's Runtime → Start timeout to 7200 s; the cluster default of 1800 s is not enough.

**Measured on 6000D**: the 1M context fits; 40 streams share a KV pool of about 3.25M tokens (about 5.1M on 96 GB cards); a single request decodes at about 185 tok/s, eight concurrent requests at about 530 tok/s in total. The template ships the autotune launch table measured on the 6000D (bit-checked; other cards fall back to the default launches); end-to-end it gains within noise for this recipe.

## Deployment requirements of the DGX Spark (GB10) x2 profile

- Select two GB10 nodes, one rank per node; a single-node deployment is refused because rank 0's address is missing. TensorFold's TP is the node count (TP2 on two nodes) whatever TP/PP the deploy form states.
- Both nodes must sit on the same Host RDMA parameter network (RoCE ports); the network supplies the interface, HCA and GID, so the template names no node interface. Without one, the deploy evaluation says why.
- Each rank needs the full weights: each downloads about 176 GB from the chosen source, or registers the same self-contained snapshot directory on its node.
- Measured on two GB10 nodes with the ModelScope weights: healthy about 6.5 minutes after start; four short streams at about 100 tok/s in total; a 94K-token prompt prefills at about 1,750 tok/s.

## Configurable arguments

A callable profile has no parameter dictionary. Add command-line arguments in the deploy form's custom arguments: the adapter passes them to every rank's `tensorfold serve`, and a repeated argument takes its last value, so they replace the template's defaults. Set environment variables in the Runtime section's environment rows; a name the template also sets takes your value.

| Argument | GB10 x2 default | RTX PRO 6000 / 6000D x4 default | Meaning |
|---|---|---|---|
| `--context N` | `1048576` | `1048576` | One request's window, prompt plus reply; at most 1048576 |
| `--parallel N` | `4` | `40` | Requests decoded together, 1–64; they share one KV pool |
| `--max-tokens N` | `32768` | `32768` | The reply cap when a request sends no `max_tokens` |
| `--thinking` / `--no-thinking` | on | on | Whether to think by default; a request overrides it with `chat_template_kwargs.enable_thinking` |
| `--reasoning-effort low\|medium\|high\|xhigh` | `medium` | `medium` | Reasoning effort when thinking; on GLM-5.3 `medium` is High and `xhigh` is Max; a request overrides it with `reasoning_effort` |
| `--vision` | on | on | Accept image and video input; add `--vision-urls` to also accept public HTTP(S) image URLs (data URLs only by default) |
| `--drafter /draft` | DFlash2 | DFlash2 | The draft model; `--drafter none` plus `TF_GLM_MTP=1` uses the checkpoint's own MTP head, which needs `--parallel 1` |
| `TF_GLM_CACHE_GIB` | — | `60` | Cap of each card's KV pool in GiB; the engine shrinks it to what the card has left after weights and the reserve |
| `TENSORFOLD_MEMORY_RESERVE_GIB` | — | `7.5` | VRAM left free on each card in GiB, at least 2 |
| `TF_GLM_DENSE` | `q4` | `fp8` | Dense precision: `q4` is faster with lower fidelity (73% first-token agreement with BF16, versus 86% for `fp8`); `bf16` needs the most memory |
| `TF_GLM_KV` | `fp8` | `fp8` | KV precision; `bf16` halves the pool |

**How the context works and what limits it** (the measured figures below are from the RTX PRO 6000 / 6000D profile):
- One request is capped at 1048576 tokens.
- All requests share one KV pool, sized to the smaller of `TF_GLM_CACHE_GIB` and each card's VRAM minus weights and the reserve. The engine shrinks the pool to fit rather than running out of memory. Measured: about 5.1M tokens on 96 GB cards, about 3.25M on the 84 GB 6000D.
- An explicit `--context` that cannot fit even one request fails at start and reports the largest window that fits.
- A request's `max_tokens` caps one reply, falling back to `--max-tokens` when the request sends none. A reply never exceeds the context minus the prompt.

**Cards and memory**:
- The RTX PRO 6000 / 6000D profile is four cards on one host, TP4, with at least 80 GiB each. The placement resolver checks this, and the deploy evaluation refuses anything else.

The DFlash2 draft weights are CC BY-NC-ND 4.0 (non-commercial).
