# Captured engine responses

Real responses captured on gpu3 on 2026-09-05 from a running vllm-omni
instance (deployment `qwen3-tts-12hz-0-6b-base`, spec
`bf16-base-0-6b-vllm-nvidia-turing`, args `--omni --dtype half`, port 30001):

| file | source | note |
|---|---|---|
| `vllm-omni-cuda12.9-vllm0.25.1.json` | `GET /openapi.json` | image `quay.io/gpustack/runner:cuda12.9-vllm0.25.1@sha256:0f64a3615f7019fccc5a85c59ba1671731638979bd6320f260496cb446b058ba`; OpenAPI 3.1, 38 paths, 373 schemas. `/v1/audio/speech` → `OpenAICreateSpeechRequest`, `required: [input]`, `task_type` enum `CustomVoice|VoiceDesign|Base`, plus MOSS-TTSD/SoundEffect fields in the same schema. |
| `voices-qwen3-tts-base-empty.json` | `GET /v1/audio/voices` | Base checkpoint: no preset speakers. |

Plan P1 (`artifacts/archive/2026-08-21-playground/plan-request-composition.md`) serves these
from the fake-engine `GET /openapi.json` and `GET /v1/audio/voices` routes and
uses them in `inferenceapi` OpenAPI-skeleton tests. Re-capture when the
acceptance image changes and record the new digest here.
