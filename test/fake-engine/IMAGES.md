# Image publication record

## Republished 2026-10-17

`:test` on Docker Hub and Quay was rebuilt from `test/fake-engine` at `390127b8d`
and now points to the same multi-architecture index in both:

```text
docker.io/wangweinoo1/fake-engine:test@sha256:8890fea5476f01a541a9de46f4b2f2969f171b144403e9a054295596f1f03abc
quay.io/zstackai/fake-engine:test@sha256:8890fea5476f01a541a9de46f4b2f2969f171b144403e9a054295596f1f03abc
```

- `linux/amd64`: `sha256:db3ebea105ae39bff01d6d6438a9a741110de32954ffdf791afda98596d51ed5`
- `linux/arm64`: `sha256:0bea573a8007eaccd61ff78f9198566f29e8b763f4f0f0dae20a5a067d75b64e`

Why: the 2026-08-22 image below answered `/v1/models` with the model path
(`/model`) whatever `--served-model-name` said. Since 2026-10-03 the agent
refuses an engine whose served names lack the instance's own (`ENGINE_PORT_IN_USE`),
so the RunPod `SeedInferenceTemplateFake` case failed on every run. The new image
answers under the served name. It was built with `crane append` and `crane mutate`
(no Docker build), one layer per architecture holding `fake-engine` and its
`vllm`/`sglang` hard links.

The record below is the 2026-08-22 publication, kept for the history; its digests
no longer back `:test`.

The image is intentionally built locally by `make fake-engine-image` and
exported to `fake-engine.tar`. A local Docker Desktop arm64 build has been
exercised; local BuildKit attestation digests are environment-specific.

Quay publication (2026-08-22): [zstackai/fake-engine](https://quay.io/repository/zstackai/fake-engine)

```text
quay.io/zstackai/fake-engine:test@sha256:d74179cbfc883530e90dc63c1c3242146f3ed2b319fa6145e97e9891ecaaa2f6
```

The repository allows anonymous pull and is the acceptance source for hosts
that cannot reach Docker Hub directly.

This digest is a multi-architecture OCI index containing:

- `linux/arm64`: `sha256:c2f594b7480de1ec20a2296a68da7187ddd9dc2c9da2e06f75bc6de798dfcccb`
- `linux/amd64`: `sha256:9dd78018b2253a092b24408c361fe3969fc95af35821163d92bc2ac53743dd03`

Use the immutable digest in CI rather than the mutable `:test` tag. The old
`:tagname` tag is left in place for now and points to the same multi-arch index.

Docker Hub publication (2026-08-22): [wangweinoo1/fake-engine](https://hub.docker.com/repository/docker/wangweinoo1/fake-engine)

```text
docker.io/wangweinoo1/fake-engine:test@sha256:d74179cbfc883530e90dc63c1c3242146f3ed2b319fa6145e97e9891ecaaa2f6
```

The Docker Hub manifest has the same arm64 and amd64 child manifests as the
Quay publication above.

The anonymous Docker Hub proxy below was also verified to serve the same OCI
index digest. It is a fallback access path, not a separately published image:

```text
dockerproxy.net/wangweinoo1/fake-engine:test@sha256:d74179cbfc883530e90dc63c1c3242146f3ed2b319fa6145e97e9891ecaaa2f6
```
