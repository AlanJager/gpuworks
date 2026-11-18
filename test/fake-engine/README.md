# fake-engine

Static test engine shared by L1 fake drivers and optional L3 Docker tests. It
validates that the mounted model contains `config.json`, records argv at
`fake-engine-argv.json` in the standard temporary directory (`/tmp` by default), and serves the health/OpenAI/sleep endpoints.
Its protocol fixture covers chat and text completions (sync and SSE), embeddings, rerank, audio
transcription and speech, image generation, and the asynchronous video
create/poll/content/cancel lifecycle, and vLLM's root `/tokenize` and
`/detokenize` (prompt or chat messages; a byte-level tokenizer, with vLLM's
reply keys, defaults and error bodies). Invalid methods, encodings, and required
fields return OpenAI-shaped errors so upper-layer tests cannot pass against an
unconditionally successful stub.

Build locally with `make fake-engine`; build an optional image with
`make fake-engine-image`. It is test infrastructure and must not be used as a
production inference image.

Set `FAKE_ENGINE_STARTUP_DELAY`, `FAKE_ENGINE_CRASH_ON_START`,
`FAKE_ENGINE_HEALTH_503`, or `FAKE_ENGINE_OOM_EXIT` to exercise the matching
reconcile/error paths. `--dialect vllm|sglang` is accepted for both CLI
families; all arguments are recorded for assertions.

## Generation fixture contract

Both `POST /v1/chat/completions` and `POST /v1/completions` honor `stream`.
Streaming chat uses `choices[].delta`; streaming text completions use
`choices[].text`. Both send an SSE stop event followed by `[DONE]` and stop
waiting when the request is canceled. Missing messages/prompt, malformed JSON,
and invalid stream-option types return structured errors.

`FAKE_ENGINE_RESPONSE_TEXT` configures the response body. For streaming replies,
the configured text is repeated `FAKE_ENGINE_STREAM_CHUNKS` times (default 1).
`FAKE_ENGINE_REASONING_TEXT` optionally adds chat `reasoning_content` before the
body, in both synchronous and streaming replies. It is absent by default and
is never added to text completions. `FAKE_ENGINE_TTFT_DELAY` and
`FAKE_ENGINE_TPOT_DELAY` accept Go durations for first-response and between-chunk
waits. These controls exercise parsers and timing paths; chunks are not tokens.

Nonstream replies contain configured synthetic usage. Streams emit a final
empty-choices usage event only with `stream_options.include_usage: true`;
omitted or false does not emit usage. Supplying non-null `stream_options` with
`stream: false` is rejected, so clients must omit that field for synchronous
requests. `FAKE_ENGINE_PROMPT_TOKENS`, `FAKE_ENGINE_COMPLETION_TOKENS`, and
`FAKE_ENGINE_CACHED_TOKENS` set synthetic totals (defaults 16, 8, and 0).
`continuous_usage_stats` is accepted and type-checked for the console's default
request, but this fixture only reports final totals: it does not simulate
intermediate cumulative usage or reasoning-token accounting. No token count is
inferred from character or chunk counts.

## Public images

The same small image is published to both registries for optional network
smoke tests:

- [Docker Hub: `wangweinoo1/fake-engine`](https://hub.docker.com/repository/docker/wangweinoo1/fake-engine)
- [Quay: `zstackai/fake-engine`](https://quay.io/repository/zstackai/fake-engine)

Use the immutable multi-architecture digest recorded in
[IMAGES.md](IMAGES.md) when a test or release job pulls from a registry. The
ordinary L3 tests build/load the image locally and therefore remain hermetic.

Source changes require a new image build. The recorded public images predate
the text-completion streaming and reasoning additions; they are not updated by
building locally. See the publication record before relying on a remote tag.
