# Qwen3.8-Flash-Next 125B (GGUF · llama.cpp)

Runs the 125B Qwen3.8-Flash-Next on an Apple silicon Mac. The engine is upstream llama.cpp, which supports the model's architecture (qwen4exp, its n-gram embedding table included) since b10660; the environment package is the b11617 macOS release. The weights are the ones the Strata template uses on NVIDIA cards: ISTA-DASLab's GSQ-RCO IQ2_XS quantization, about 68 GB in two shards.

## Two specs

| Spec | For | Admission (memory for Metal) |
|---|---|---|
| `iq2xs-llamacpp-apple-128g` | a 128 GB Mac (default working set about 96 GiB) | 80 GiB |
| `iq2xs-llamacpp-apple-64g` | a 64 GB Mac (default working set about 48 GiB) | 44 GiB |

Both use the same weights. llama.cpp keeps tensors over 4 GiB on the SSD and reads them on demand (`--lazy-mode auto`, its default), and this GGUF keeps the n-gram table in a shard of its own (28.8 GB), so what stays resident on the GPU is mainly the first shard (39.2 GB) and the KV cache. The KV cache is small: 12 of the 48 layers attend fully, about 24 KiB per token, 3 GiB for a 128K context.

Measured on an M4 Max Mac Studio with 128 GB: about 39 GiB in use on the GPU, decode 35–45 tok/s, prefill about 550 tok/s on an 8K-token prompt. With the same machine's `iogpu.wired_limit_mb` set to 49152 to stand in for a 64 GB Mac: 39.3 GiB in use, same speed. A real 64 GB Mac has less RAM, so more n-gram reads go to the SSD (a few KB per token); its speed has not been measured on such a Mac. Keep at least 70 GB of disk free.

## Parameters

A callable spec has no parameter dictionary: append llama-server options under Custom arguments on the deploy form; for a repeated option the later one wins. The template sets `-c 32768` (context), `-ngl 99` (every layer on the GPU) and `--jinja` (the chat template inside the GGUF, with tool calls and reasoning). A larger context takes more memory for the KV cache.

## Known limits

- Upstream's n-gram embedding convolution carries no state across ubatches, so it is exact only for prefills starting at position 0; chunked prefill state is marked unfinished upstream.
- Text input only: the vision projector is not part of the download.
- Engine sleep and multi-node inference are not available on Mac nodes.
