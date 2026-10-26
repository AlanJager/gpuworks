# Captured engine responses

Real responses captured on gpu3 on 2026-09-05 from a running vllm-omni
instance (deployment `qwen3-tts-12hz-0-6b-base`, spec
`bf16-base-0-6b-vllm-nvidia-turing`, args `--omni --dtype half`, port 30001):

| file | source | note |
|---|---|---|
| `vllm-omni-cuda12.9-vllm0.25.1.json` | `GET /openapi.json` | image `quay.io/gpustack/runner:cuda12.9-vllm0.25.1@sha256:0f64a3615f7019fccc5a85c59ba1671731638979bd6320f260496cb446b058ba`; OpenAPI 3.1, 38 paths, 373 schemas. `/v1/audio/speech` → `OpenAICreateSpeechRequest`, `required: [input]`, `task_type` enum `CustomVoice|VoiceDesign|Base`, plus MOSS-TTSD/SoundEffect fields in the same schema. |
| `voices-qwen3-tts-base-empty.json` | `GET /v1/audio/voices` | Base checkpoint: no preset speakers. |

The captured document remains unchanged. The fake-engine `GET /openapi.json`
response preserves its paths and components and adds
`fake-transcription-path.json`, the schema for the fake engine's implemented
`POST /v1/audio/transcriptions` handler: multipart `model` and `file` are
required and the JSON response contains `text`. The captured TTS engine does
not advertise this ASR route, so its schema alone does not describe every
operation implemented by the shared fake handler.

The captures also feed `inferenceapi` OpenAPI-skeleton tests. Re-capture when
the acceptance image changes and record the new digest here.
