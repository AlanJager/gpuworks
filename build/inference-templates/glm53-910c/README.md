# GLM-5.3-Flash Ascend template asset

This directory owns the reviewable patches and launcher for the pinned ARM64
vLLM Ascend distribution. It builds the catalog's `assets/runtime.tar.gz`;
it does not build or publish an inference image.

`manifest.json` fixes the base image ID and the original and patched hashes of
17 modules. The payload is byte-identical to the modules used in the measured
16-device configuration. At startup, `install.py` checks every input before
copying the modules into the container's writable layer. A different base file
or damaged payload aborts startup. Already installed modules allow a restart.

Pull the image named in the manifest, then run from the repository root:

```bash
python3 build/inference-templates/glm53-910c/build.py
python3 -B -m unittest discover -s build/inference-templates/glm53-910c -p 'test_*.py'
```

Use `--image LOCAL_ALIAS` to reuse the exact imported image; the image ID is
still checked. `--base-root DIRECTORY` accepts previously extracted original
modules without Docker; every module's base hash is still checked. No image
process needs to run to rebuild the asset, so extraction works across host CPU
architectures. The build requires Python 3, Docker (unless using `--base-root`)
and `patch`. Archive paths, ordering, modes and timestamps are deterministic.

After rebuilding, update the catalog asset's SHA-256 and size in
`internal/server/inferencecatalog/seed/catalog/models/gpuworks/glm-5.3-flash-910c/model.yaml`.
The package checksum and payload hash checks serve different purposes: the
first protects transport; the second rejects an incompatible base image.
