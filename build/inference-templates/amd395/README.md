# AMD gfx1151 launch overlays

This package contains AMD launch recipes for Qwen3-0.6B and Qwen3.8-27B. It is
source input for `prepare_import.py`, not an importable inference catalog.
Model configuration, modalities, KV metadata, weight sizes, repositories and
revisions come from the current built-in seed catalog. Each overlay selects an
exact `seed_spec`; a missing or renamed seed spec fails instead of falling back.
The generated package contains only the AMD specs, so importing it does not
replace the seed's NVIDIA or Ascend launch recipes.

The overlays may define AMD engine commands, environment, placement and API
settings as well as image, GPU selector and arguments. Qwen3.8-27B has no seed
SGLang spec, so mechanically inheriting its NVIDIA vLLM launch command would be
incorrect. The shared materializer copies only `weights` and `weight_bytes`
from the selected seed spec, copies model facts from that model, and rejects
model or weight facts in an overlay.

Every recipe requires exactly one AMD GPU with reported architecture gfx1151.
The name predicate accepts Radeon 8060S, Ryzen AI Max+ 395, and the probe fallback
`GPU 0x1586 (gfx1151)`. A missing or different architecture is rejected even if
the name matches; the fallback name alone does not establish the exact APU SKU.
These recipes do not treat system RAM as available GPU memory.

## Prepare an import

Python 3 with PyYAML is required. On the target node, first load the explicitly
selected image archives into its authorized existing runtime. Supply a JSON
mapping for every placeholder in `image-import.json` (including historical
SGLang r2, retained as an optional rollback image):

```json
{
  "__AMD395_VLLM_IMAGE_MAPPING_REQUIRED__:required": {
    "ref": "verified.example/amd:vllm",
    "image_id": "sha256:<64 lowercase hexadecimal digits>"
  },
  "__AMD395_SGLANG_R2_IMAGE_MAPPING_REQUIRED__:required": {
    "ref": "verified.example/amd:sglang-r2",
    "image_id": "sha256:<64 lowercase hexadecimal digits>"
  },
  "__AMD395_SGLANG_R5_IMAGE_MAPPING_REQUIRED__:required": {
    "ref": "verified.example/amd:sglang-r5",
    "image_id": "sha256:<64 lowercase hexadecimal digits>"
  }
}
```

```sh
python3 build/inference-templates/amd395/prepare_import.py \
  --docker-host unix:///run/docker.sock \
  --mapping mapping.json --output /path/to/new/delivery
```

The tool performs only `image inspect` against the explicit endpoint. It never
pulls, builds, publishes or starts a daemon. Every exact image ID must match
before any output appears. Invalid mappings, missing seed specs and write
failures leave no partial package. Existing output directories are rejected.
Unpullable placeholders must never be replaced in the source tree.

Import `delivery/image-import.json` through the image page or
`POST /api/v1/images/import`, then import `delivery/catalog/` as an inference
package. Its image entries are custom additions, not a partial system catalog.
`delivery/source-sha256.json` records hashes of the actual seed models, overlays,
image metadata and configuration used; `image-mapping.json` records the verified
image IDs. These hashes identify source input and do not prove GPU acceptance.
Checking a build daemon does not provision another node.

The earlier `build/inference-images/amd395/prepare_import.py` path has moved to
`build/inference-templates/amd395/prepare_import.py`. Source model YAML is now a
launch overlay; only the generated catalog has complete model facts. Update
packaging scripts to use the new command and generated output.

## Historical hardware boundary

The historical AMD395 r2/r5 images were local validation artifacts. Their r1
parent has no complete public rebuild recipe, so no reproducible r1→r2→r5 chain
or ability to reproduce the old image IDs is claimed. The runner matrix's
MI300X recipes are separate and do not rebuild these gfx1151 images.

The retired AMD395 host completed limited single-GPU BF16 text smokes for both
models with vLLM r2 and SGLang r5 (eight short SGLang requests per model).
Model-local English and Chinese notes retain those historical pinned revisions
and settings. Current seed revisions may differ: historical results do not
validate a newly generated package. No current AMD device, image publication,
answer-quality, long-context, image-input, sustained-performance or multi-GPU
acceptance is implied. Follow the [hardware guide](../../../docs/dev/amd-hardware-validation.md)
for a fresh acceptance run.

## Offline tool checks

With Python 3 and PyYAML installed, run `make test-amd-import-tools`. This
explicit lane tests materialization, image inspection fixtures and failure
cleanup without a daemon. The ordinary Go suite validates the overlay/seed
launch contracts without invoking Python or requiring PyYAML.
