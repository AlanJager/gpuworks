# MI300X ROCm build and import candidates

The runner matrix at `../../runner-images/matrix.yaml` is the sole source of
base-image digests, engine versions and framework versions. `build-config.json`
only selects exact matrix IDs and local output tags. The shared recipes,
repairs and audit live in `../../runner-images/rocm/`; this directory has no
second Dockerfile or copies to synchronize.

Both ROCm matrix entries remain unpublished `catalog: false` candidates.
The SGLang base targets MI300X gfx942. These recipes neither reproduce the
historical AMD395 gfx1151 images nor establish GPU compatibility or anonymous
registry availability. Building or auditing a candidate is not hardware
acceptance. Publication requires separate authorization and validation.

## Build on an authorized remote build host

Python 3 with PyYAML, an existing authorized Docker runtime, sufficient disk
space and an explicit endpoint are required. This tool does not start a daemon.
A separate daemon must already be in a verified independent network namespace
or VM before use. A separate socket or data directory alone is not isolation.
For this scoped export tool, DockerRootDir must equal OUTPUT/daemon-data; it
rejects shared runtime storage and inherited Docker contexts.

```sh
DOCKER_HOST=unix:///path/to/authorized-build.sock \
  python3 build/inference-templates/rocm/build.py --output /remote/new-output
```

The tool builds sequentially using the runner Dockerfiles, audits each image,
checks export space, writes compressed Docker exports and SHA-256 receipts.
Failures retain bounded diagnostic files and scoped images for explicit owner
cleanup; it does not run prune or operate unrelated containers. `--engine`
selects one engine; `--base-registry` selects a mirror while preserving the
matrix repository and digest. No image build was required to test these tools.

## Prepare an import

The Qwen3-0.6B files under `catalog/` are AMD launch overlays, not complete
import packages. Model facts and BF16 weight facts are materialized from the
current seed spec `bf16-sglang-nvidia-general`; seed-spec renames fail closed.
The launch command remains AMD-specific and may select vLLM or SGLang.
GPU eligibility requires AMD vendor, MI300X name and architecture gfx942.

Create `mapping.json` with exactly the two placeholder keys in
`import-config.json` (the `placeholder` fields). Each value must contain `ref` (the target-visible local
image reference) and `image_id` (`sha256:` plus 64 lowercase hexadecimal digits).
Then run against the actual target node's existing runtime:

```sh
python3 build/inference-templates/rocm/prepare_import.py \
  --docker-host unix:///run/docker.sock \
  --mapping mapping.json --output /path/to/new/delivery
```

Every image ID is inspected before output is written. The tool never pulls,
builds, publishes or starts a daemon. Import the generated
`delivery/image-import.json` as custom image entries and `delivery/catalog/`
as the inference package. The source placeholders are deliberately unpullable.
The source SHA-256 receipt identifies actual seed and overlay input; it does
not establish GPU acceptance. Current seed revisions require new hardware
validation even when historical launch settings are retained.

The earlier `build/inference-images/rocm` path has moved here. Scripts must use
this new path and package the generated catalog rather than the source overlay.

## Offline tool checks

With Python 3 and PyYAML installed, run `make test-amd-import-tools`. This
explicit lane tests materialization, image inspection fixtures and failure
cleanup without a daemon. The ordinary Go suite validates the overlay/seed
launch contracts without invoking Python or requiring PyYAML.
