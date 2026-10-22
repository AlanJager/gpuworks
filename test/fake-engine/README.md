# fake-engine

Static test engine shared by L1 fake drivers and optional L3 Docker tests. It
validates that the mounted model contains `config.json`, records argv at
`/tmp/fake-engine-argv.json`, and serves the health/OpenAI/sleep endpoints.
Its protocol fixture covers chat (sync and SSE), embeddings, rerank, audio
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

## Public images

The same small image is published to both registries for optional network
smoke tests:

- [Docker Hub: `wangweinoo1/fake-engine`](https://hub.docker.com/repository/docker/wangweinoo1/fake-engine)
- [Quay: `zstackai/fake-engine`](https://quay.io/repository/zstackai/fake-engine)

Use the immutable multi-architecture digest recorded in
[IMAGES.md](IMAGES.md) when a test or release job pulls from a registry. The
ordinary L3 tests build/load the image locally and therefore remain hermetic.
