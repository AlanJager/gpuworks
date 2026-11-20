# GLM-5.3-Flash Ascend 910B template asset

This directory owns the reviewable patches, launcher and start-up checks for
the GLM-5.3-Flash 910B profile (`w8a8-vllm-910b-graph-tp8`). It builds the
catalog's `assets/runtime-910b.tar.gz`; it does not build or publish an
inference image.

`manifest.json` fixes the base image (the pinned multi-arch vLLM Ascend index;
`image_id` is its ARM64 manifest) and the original and patched hashes of 21
modules, three of them new. At startup `install.py` checks every input before
copying the modules into the container's writable layer: a different base file
or a damaged payload aborts startup, and already installed modules allow a
restart. The overlay covers target hidden-state capture, hybrid-cache handling,
pure-text weight mapping, bounded indexer RoPE, container-local CANN extension
initialization, and two workarounds for running under a PRoot tracer (udocker),
switched on by `GPUWORKS_RUNVEIL_BG_THREAD_BALANCE` and
`GPUWORKS_RUNVEIL_WAITPID_WORKAROUND`. They are not equivalent in scope:
`BG_THREAD_BALANCE` looks for a PRoot tracer and returns without doing anything
when there is none, whereas `WAITPID_WORKAROUND` acts on the environment
variable alone — with it set, a native container also gets the ninja build
runner that reads `.ninja_log` instead of waiting on the child, and the
CPU-computed RoPE cache. **The catalog profile sets only
`WAITPID_WORKAROUND`**: the embedded PRoot serves its tracees fairly on its own
now (`build/runtime-assets/proot/README.md`), which is what `BG_THREAD_BALANCE`
worked around, so its module stays in the overlay but inert, and can be turned
back on from the deployment form. The DFlash2 proposer modules are part of the
same overlay but the profile does not enable a draft.

`serve.py` reads the mounted checkpoint's `text_config` and passes it to
`vllm serve` as `--hf-overrides`, so the profile never carries its own copy.
`warmup.py` loads the CANN `key_pool` / `pool_key_indexer` extensions and runs
the patched weight loader against tiny CPU tensors before vLLM starts.

Pull the image named in the manifest, then run from the repository root:

```bash
python3 build/inference-templates/glm53-910b/build.py
python3 -B -m unittest discover -s build/inference-templates/glm53-910b -p 'test_*.py'
```

Use `--image LOCAL_ALIAS` to reuse the exact imported image; the image ID is
still checked. `--base-root DIRECTORY` accepts previously extracted original
modules without Docker; every module's base hash is still checked (the files
can be read straight from the image layers, which is how the checked-in asset
was built). The build requires Python 3, Docker (unless using `--base-root`)
and `patch`. Archive paths, ordering, modes and timestamps are deterministic.

After rebuilding, update the catalog asset's SHA-256 and size in
`internal/server/inferencecatalog/seed/catalog/models/zai-org/glm-5.3-flash/model.yaml`.
