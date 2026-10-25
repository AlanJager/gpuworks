# GLM-5.3-Flash / Ascend 910C

This template runs integrated vLLM with W8A8 target weights and the Modal
DFlash5 draft for mixed Chinese, English and code contexts. Prefer the
**16-logical-NPU optimized profile**. The 8-device profile reduces device count
and concurrency. Both require one ARM64 node and at least 64 GiB per logical NPU.

| Logical NPUs | Profile | Engine parallelism | Running sequences per DP | Use |
| --- | --- | --- | --- | --- |
| 16 (8 dual-die cards) | optimized / throughput | TP2, DP8, EP16 | 4, 32 total | Recommended tested sustained long-context configuration |
| 8 (4 dual-die cards) | compatible / balanced | TP4, DP2, EP8 | 2, 4 total | Lower device count and concurrency |
| 4 (2 dual-die cards) | Unsupported with these weights | — | — | 256 GiB total is below the approximately 309 GiB W8A8 weights alone |

Four devices require separate W4 quantization or CPU-offload work, including
compatible weights and validation. Changing TP to four does not make this
recipe fit. The 8-device profile does not promise 32 long-context sessions or
the 16-device performance numbers.

## Deployment

Use exclusive devices on one node with one replica. Set the served model name
to **`glm53-flash-910c-dflash`**, matching the template's `--served-model-name`.
The platform supplies the bind address and port. Allow at least 1800 seconds
for cold startup.

The platform engine is callable (custom launch); the actual process is vLLM.
The generic vLLM dictionary does not accept this Ascend version's `int8` KV
cache, so the template owns the full command. Health, chat and vLLM metrics
are declared. The generic allocation's device count is not the engine's TP;
consult the table and rendered command. Do not add `--enforce-eager` or override
parallelism, graph capture or speculation settings.

Both profiles allow 139264 total tokens: 131072 input plus up to 8192 output.
Shared memory is 128 GiB. The scheduling budget of 57672 MiB per device is
64 GiB × 0.88 rounded up, not another memory measurement. The platform handles
devices, drivers and communication mounts. These are text-only, integrated,
single-node recipes; multi-node and PD disaggregation are not supported.

## Pinned dependencies

- Target: ModelScope `Eco-Tech/GLM-5.3-Flash-w8a8` at
  `fce3173cef79062469745b520c6742883e60a166`, approximately 331.8 GB, mounted at `/model`.
- Draft component: Hugging Face `modal-labs/GLM-5.3-Flash-DFlash` at
  `dae6d319510889a7f1ac5242d5b7fb2d5d95eb05`, approximately 2.78 GB, mounted at `/draft`.
- Base: `quay.io/ascend/vllm-ascend@sha256:89f41d4b45df14461cb861e8844cc3ae2945295f1e4ca3888ec8a4725d792f93`,
  the ARM64 manifest of v0.30.0rc2-a3.
- Asset: 17 adapted modules and the launcher travel with the complete catalog
  package. Startup verifies all original and resulting hashes before installing
  into this container's writable layer. Identical existing patches allow restart;
  an unexpected base is refused. Copying the YAML alone omits the asset.

The repository's `build/inference-templates/glm53-910c/` contains reviewable
patches, the checksum manifest and a deterministic rebuild command. No new base
image needs publishing.

## History caching and performance

Keep a session's priming and subsequent requests on the same DP rank. The
measurements use the `X-Data-Parallel-Rank` HTTP header: ranks 0–7 on 16 devices,
0–1 on 8 devices, with sessions evenly assigned. Verify forwarding when using a
gateway; the template does not add session routing.

Read actual caching from `usage.prompt_tokens_details.cached_tokens`. The
16-device run reused 95744 tokens from a planned 98304-token history, leaving
35328 new tokens in a 131072-token input. Do not report this as an exact 96K
cache hit. Output rates include reasoning tokens.

Existing 16-device measurements used loopback requests, 32 independent histories,
mixed Chinese/English/code and sampling. With 4096 output tokens per request,
32/32 passed protocol and caching checks. Per-request decode minimum/median/maximum
was **35.96/41.50/50.90 TPS**; 21/32 reached 40 TPS. Whole-request wall-clock output
throughput, including prefill, was **853.21 TPS**. The simultaneous 32-request
decode window yielded 1352.11 TPS with a different denominator.

The sustained run spread the first arrivals across 60 seconds and used two
rounds per session with 2048 output tokens per round. All 64 requests retained
history. Per-request minimum/median/maximum was **14.78/20.48/40.31 TPS**, and
whole-run wall-clock throughput was **470.91 TPS**. Both workloads had no
preemptions and passed subsequent known-answer checks. The default 2048-token
prefill chunk outperformed the tested 4096-token chunk's sustained 450.27 TPS;
batch peaks are not sustained service rates.

The goal of 32 sessions each consistently above 40 TPS remains unmet. These
results are not production-gateway guarantees or guarantees for other content,
output lengths or arrival patterns.
