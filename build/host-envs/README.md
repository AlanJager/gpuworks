# Environment packages

Engines on macOS nodes run as host processes (the agent's `host` runtime driver), because a Linux
container on macOS runs in a VM without the Metal GPU. What a deployment names as its image is an
**environment package**: an OCI image, built for `darwin/arm64`, whose files are a relocatable
root the agent unpacks once per manifest and starts the engine from.

`build.sh` builds one on Linux; no Mac or container daemon is needed:

```bash
build/host-envs/build.sh vllm-metal 0.31.0 r2 vllm-metal.tar
crane push vllm-metal.tar <registry>/gpuworks/gpuworks-runner:metal15.0-vllm0.31.0-r2
```

The revision is the one the runner matrix records for the package
(`build/runner-images/matrix.yaml`, `builder: host-env`); it is given on every
build so a changed recipe never reuses a published tag.

Engines: `vllm-metal` (version = its GitHub release), `mlx-vlm` and `mlx-audio` (PyPI versions), and
`llama.cpp` (its `bNNNNN` build; upstream's macOS release, native code with no Python).
Packages the inference templates use are published as `gpuworks/gpuworks-runner:metal<minimum macOS>-<engine><version>-rN`
in every registry the catalog lists (Docker Hub, quay.io, SWR, registry.zstack.ai), like the Linux runners.

## Layout

| Path | Content |
|---|---|
| `/python` | python-build-standalone CPython for `aarch64-apple-darwin`; the engine and its dependencies are installed into its own `site-packages`, so there is no venv. Absent from the llama.cpp package |
| `/bin/<launcher>` | `exec python3 -m <module> "$@"`: `vllm`, `mlx_vlm.server` and `gpuworks-mlx-vlm`, `mlx_audio.server` and `gpuworks-mlx-audio` (the serve adapters in `adapters/`, which pin the instance to its `--model`). The llama.cpp package has upstream's `llama-server` and its dylibs here instead |
| `/vllm-workspace/examples` | vllm-metal only: vLLM's bundled chat templates, in the working directory the Linux runners use |
| `/gpuworks-env.json` | what was built: engine, version, Python, minimum macOS |

The image config sets `PATH=/bin:/python/bin:/usr/bin:/bin` and `PYTHONNOUSERSITE=1` (`PATH=/bin:/usr/bin:/bin`
for llama.cpp), and the labels `ai.gpuworks.host-env.engine` / `ai.gpuworks.host-env.version`.

The adapters' tests run with `make test-unit` (`python3 -m unittest` in `adapters/`).

## Contract with the agent

- Absolute paths in the package mean paths inside it, as in a container. The agent re-roots absolute
  symlinks and a venv's `pyvenv.cfg` on unpack, and translates the package paths a launch names (argv,
  environment, `PATH`, working directory) to the unpacked root. Mount targets (`/model`) become their
  host sources.
- A package must not need the network at run time. Optional dependencies a model imports on first use
  are installed at build time (mlx-audio's `misaki[en]` and spaCy model); undeclared ones are added
  (vllm-metal's `gguf`).
- The agent gives each instance its own `HOME` and `TMPDIR`, a per-package cache (`XDG_CACHE_HOME`,
  `TORCHINDUCTOR_CACHE_DIR`), and none of the agent's `PATH`, `DYLD_*` or Python variables.
- Inputs are pinned in `build.sh` (Python build, uv, minimum macOS); the layer is built with sorted
  entries, root ownership and a fixed mtime so equal inputs give an equal layer.

The design and the measurements behind it: `artifacts/work/2026-10-13-apple-silicon-nodes/`.
