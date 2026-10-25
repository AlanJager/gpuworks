# MiniMax H3

MiniMax H3 generates audio and video. GPUWorks can submit an asynchronous video job from the Playground, poll its status, and download the completed result.

## Ascend profile

The current Ascend profile was verified on four 910B4 GPUs with 32 GiB each. The template selects the repository's `FL2VA` directory while preserving its repository-relative layout, then launches SGLang Diffusion with TP4 and SP1. Users do not need to enter these details manually.

Start with a 512-pixel short edge. A verified 896×512 video of about four seconds completed with approximately 29,424 MiB peak memory per GPU.

The current profile does not support 910C/A3. Upstream MiniMax H3's SGLang diffusion full-loop denoise still requires CUDA, and changing the attention backend only moves the failure into CUDA JIT; changing the context to NPU does not remove that dependency. A 910C profile should be added only after upstream provides a working NPU denoise path and it has been verified on real hardware.

## NVIDIA profiles

The NVIDIA profiles run the GPUWorks vLLM-Omni 0.30 runner, which already includes the modular MiniMax H3 pipeline, and follow vLLM-Omni's own MiniMax H3 recipes. Each profile pins its GPU count:

| Profile | GPUs | Topology |
|---|---:|---|
| General | 1 | Layerwise offload of the DiT and text encoder (capacity path, slow) |
| Blackwell | 4 | Ulysses 4, no offload |
| RTX 4090 | 2 | TP2 with distributed layerwise offload, 12 resident DiT blocks |
| RTX 5090 | 2 | TP2 with distributed layerwise offload, 20 resident DiT blocks |
| RTX PRO 5000 | 4 | TP2 × Ulysses 2, no offload |

The offload profiles keep most weights in host memory: vLLM-Omni asks for at least 200 GiB of available system RAM, and 384 GiB is recommended. The NVIDIA profiles have not been run on GPUWorks yet.

## Known boundaries

- This profile accepts exactly four 910B4 GPUs with at least 32 GiB each. Other GPU counts and models are not inferred from a single machine result.
- A 768-pixel short edge may run out of memory during the final VAE decode.
- TP8 caused excessive host-memory pressure before weight sharding and is not published as a supported profile.
- The template downloads only the required `FL2VA` repository directory. If the same revision of the complete repository is already cached on the node, GPUWorks reuses it.
