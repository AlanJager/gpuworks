# ROCm runner build candidates

The two ROCm entries in `../matrix.yaml` own the public upstream base digests,
component versions and platform constraints. This directory is the single
implementation of ROCm preparation, repair and component auditing. The local
export tool in `../../inference-templates/rocm/` uses these same Dockerfiles and
sources; there are no copies to keep in sync.

The output names follow the official `gpuworks/gpuworks-runner` tag grammar.
Both entries set `catalog: false`: they are unpublished build candidates and
are absent from the built-in catalog and all AMD395 templates. A Dockerfile or
tag does not establish publication. Release, registry digest checks, anonymous
pull checks and hardware acceptance require separate authorization.

The SGLang base is the MI300X gfx942 line. Neither recipe reproduces the
historical AMD395 gfx1151 r2/r5 images or implies cross-device compatibility.
