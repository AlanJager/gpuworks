# ROCm runner recipes

The ROCm entries in `../matrix.yaml` own the public upstream base digests,
component versions and platform constraints; this directory is the single
implementation of ROCm preparation, repair and component auditing.

`rocm7.2.3-vllm0.30.0` is published and listed in the built-in image catalog.
Its base, the upstream `vllm/vllm-openai-rocm` image, ships kernels for gfx942
(MI300X/MI325X) and gfx1151 (Ryzen AI Max+ 395) among others; the built-in
launch specs name those two architectures. The SGLang entry is an unpublished
MI300X (gfx942) candidate with `catalog: false`.
