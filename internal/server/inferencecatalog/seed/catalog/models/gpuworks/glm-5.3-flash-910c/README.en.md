# GLM-5.3-Flash · Ascend 910C · Graph

Single-node text inference with the official vLLM Ascend image, W8A8 weights, INT8 KV cache, and decode graphs. The template starts `vllm serve` directly, without custom patches or a draft model.

| Logical NPUs | TP / DP | Maximum sequences per DP rank | Use |
| --- | --- | --- | --- |
| 16 × at least 64 GiB | TP2 / DP8 | 4 | Preferred throughput configuration |
| 8 × at least 64 GiB | TP4 / DP2 | 4 | Fewer devices |

Counts refer to devices visible to the runtime. Eight dual-die physical cards normally expose 16 logical NPUs. Four 64 GiB devices cannot hold the approximately 309 GiB W8A8 weights; there is no four-device recipe.

The context limit is 139264 tokens, allowing 128K input plus output. Prefill chunks are 2048 tokens, with a 512-token long-prefill threshold. Actual prefix hits depend on state-cache alignment and retention: priming 96K history does not guarantee only 32K uncached input on every request. Caches are independent across DP ranks; reuse also requires routing a request back to the rank holding its prefix. The eight-device recipe does not imply capacity for 32 independent long histories.

## Measured scope

Both recipes were validated on one 910C host on 2026-10-25. Each request used 131072 input tokens with 98304 independently primed history tokens and mixed Chinese, English, and code. Each test used one synchronized wave, no retries, and a 1024-token output budget; every request emitted 1024 tokens.

| Logical NPUs / concurrency | Per-request decode TPS (min / median / max) | Median time to first token | Total output / test wall time | Actual cached tokens per request |
| --- | --- | --- | --- | --- |
| 16 / 32 | 25.35 / 25.73 / 26.03 | 29.00 s | 475.91 token/s | 97152 |
| 8 / 4 | 31.10 / 31.19 / 31.29 | 12.47 s | 90.43 token/s | 97920 |

Per-request decode TPS counts tokens after the first token bundle over decode time. Aggregate output throughput includes prefill and waiting in the measured wave, excluding history priming. Actual uncached inputs were 33920 and 33152 tokens, respectively. Both recipes confirmed Graph replay on every DP rank and passed known-answer checks with cold, partially cached, and fully cached inputs. Multi-position fact retrieval passed 64/64 and 8/8 answer checks, respectively. These are single-wave checks, not sustained-load results; the target of 32 concurrent requests at 40 TPS each remains unmet.

## Image and graphs

The template pins the ARM64 digest of the official `nightly-releases-v0.30.0-a3` image built on 2026-10-24; see `model.yaml`. This is a release-branch nightly containing CANN 9.2.0. Host drivers and firmware must be compatible. It uses the V1 model runner and `FULL_DECODE_ONLY`, without `--enforce-eager`. The image includes the upstream GLM5Next convolution-state fix for padded decode graph requests.

DSA context parallel is explicitly disabled. With this pinned image and TP2/DP8, enabling it produced invalid output for a 128K known-answer probe with both cold and warm caches. The same input passed with this option disabled. Capture sizes must be divisible by TP and no larger than the per-rank sequence limit, so both recipes set `--max-num-seqs 4`.

The official single-node guide uses TP16/DP1 and includes an MTP graph configuration. This template uses a different TP/DP layout and enables neither MTP nor DFlash. Performance measurements from the earlier custom DFlash patch build do not apply to this template.

- [Official model deployment guide](https://docs.vllm.ai/projects/ascend/en/latest/tutorials/models/GLM5.3-Flash.html)
- [Official compatibility matrix](https://docs.vllm.ai/projects/ascend/en/latest/community/versioning_policy.html)
- [Padded decode graph fix](https://github.com/vllm-project/vllm-ascend/pull/18878)

The `callable` launch mode passes the Ascend-specific INT8 KV argument; the engine process is vLLM. Endpoints include `/health`, OpenAI chat streaming, and `/metrics`. The served model name is `glm53-flash-910c-stock-graph`.
