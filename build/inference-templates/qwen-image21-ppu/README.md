# Qwen-Image-2.1 editing on ZW810E

The catalog asset backports the Qwen 2.1 pipeline, transformer and VAE to the
Diffusers 0.37 shipped with the official GPUWorks `hggc13.0-sglang0.5.13-r3` PPU runner. The pipeline uses the
runner's existing named denoising cache contexts. The SGLang adapter registers a
worker-importable configuration for this model's image-edit task and defaults
the output to PNG for RGBA images.
The pre-start hook installs these files into the instance's writable layer.

The source files and Apache 2.0 license come from Hugging Face Diffusers at
`d9586a3dc62cce5091bdb7cff981d725f4a6dfe5`. To rebuild from the repository root:

```sh
curl -fL https://codeload.github.com/huggingface/diffusers/tar.gz/d9586a3dc62cce5091bdb7cff981d725f4a6dfe5 -o /tmp/qwen21-diffusers.tar.gz
python3 build/inference-templates/qwen-image21-ppu/build.py /tmp/qwen21-diffusers.tar.gz
```

The builder verifies the SHA-256 of each of the four source files and the license
before applying local changes. Recompressing the upstream archive preserves these checks.
It prints the SHA-256 and size of the adapter to
record in the model's `assets` declaration. Deployments use the bundled files
offline once the runner and model are present.
