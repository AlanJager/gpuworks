# TML Inkling 2 Mini (GGUF · llama.cpp)

Runs Thinking Machines' Inkling 2 Mini on an Apple silicon Mac, with text and image input. Inkling 2 Mini is a 26B mixture-of-experts model with about 3B parameters active per token; its 24 decoder layers run layers 6–17 three times, 48 layers in all. The engine is upstream llama.cpp, which loads this architecture (`inkling2_loop`) and its image encoder from build b12600 on; the environment package is the b12631 macOS release. The weights are ggml-org's official GGUF: the IQ4_NL model (17.6 GB) and the Q8_0 vision projector (346 MB).

## Download size

The repository keeps every quantization side by side (BF16, Q8_0, IQ4_NL and two vision projectors), and a template downloads a whole repository, so the download is 98.8 GB although the engine loads 18.0 GB of it. Keep about 100 GB of disk free.

## Spec

| Spec | For | Admission (memory for Metal) |
|---|---|---|
| `iq4nl-llamacpp-apple` | a Mac with 36 GB or more | 24 GiB |

Measured on an M4 Max Mac Studio with 128 GB at the template's 64K context: about 21 GB resident (the 18 GB of weights, memory-mapped, plus about 3.9 GB of process memory: KV cache, compute buffers); loads in about 5 seconds; decode about 35 tok/s on a short prompt and 32 tok/s at 10K tokens of context; prefill about 300 tok/s on a 10K-token prompt. A 32 GB Mac (default working set about 21 GiB) does not have room for it.

## Parameters

A callable spec has no parameter dictionary: append llama-server options under Custom arguments on the deploy form; for a repeated option the later one wins. The template sets `-c 65536` (context, shared by four parallel slots), `-ngl 99` (every layer on the GPU), `--jinja` (the chat template inside the GGUF) and `--alias` (the model answers under the deployment's served name). Most attention layers use a 1,024-token sliding window, so a larger context costs little extra memory.

## Reasoning

Replies carry the model's thinking in `reasoning_content`, separate from `content`. The chat template takes `reasoning_effort` (`none`, `minimal`, `low`, `medium`, `high` (the default), `xhigh`, `max`, or a number from 0.0 to 0.99) as a request field or in `chat_template_kwargs`. It is a hint, not a switch: with `none` the model still writes a short thought.

## Known limits

- Images only: the vision projector encodes still images, not video or audio.
- Engine sleep and multi-node inference are not available on Mac nodes.
