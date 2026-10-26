# Vendor Support Matrix

| Vendor/Framework | Detection | Full-GPU Deployment | dGPU | Topology/Notes |
|---|---|---|---|---|
| NVIDIA / CUDA | nvidia-smi, sysfs | Docker, runveil, nerdctl | HAMi libvgpu.so (x86_64, glibc ≥ 2.17) | Supports CUDA/NCCL topology; driver libraries are supplied by the host |
| Ascend / CANN | npu-smi, driver and CANN facts | Docker + Ascend runtime; runveil in some environments | vCANN; capability depends on host device-share permissions | Host and container CANN versions must not be conflated into one conclusion |
| PPU / HGGC | ppu-smi | Depends on provider/runtime | HGGC GPU memory and weighted QoS | Sustained 20/80 workloads have demonstrated throughput weighting under load, not hard SM partitioning |
| Hygon / DTK | hy-smi | Determined by the catalog spec and runtime | Native Z100 vdev (experimental, x86_64 + Docker) | Requires the optional native helper, host libraries and a per-GPU behavioral probe; other models remain full-GPU only |
| Apple / Metal | sysctl, ioreg (macOS 15+, Apple silicon) | host (the engine is a host process; the image is a darwin environment package) | None; a Mac runs multiple instances concurrently using unified memory | One unified-memory card; GPU memory is the Metal working set; engine sleep, multi-node operation, and the KV disk tier are unsupported; see [ADR 0044](../adr/0044-mac-engines-as-host-processes.md) |

The dGPU capability status in the table above is derived from the server's partition capability assessment:

- `ready`: dGPU is available.
- `unavailable`: dGPU is unavailable, with a `reason` explaining why, such as “Missing dGPU assets” or “dGPU probe failed”.

When dGPU is unavailable, the node can still use full-GPU deployment. The platform does not silently start instances that fail dGPU requirements with unlimited GPU memory instead.
For tested Ascend combinations, CANN, and runtime details, see the [Ascend Matrix](ascend-support-matrix.md). For outstanding PPU work, see the [Development Ledger](../dev/ledgers/ppu-support-gaps.md).


## PPU device identity and dGPU probes

PPU injection uses full UUIDs returned by `ppu-smi` and verified against device nodes, writing them to `CUDA_VISIBLE_DEVICES` in scheduling order. HGGC maps those UUIDs to compute devices `0..N-1`; Docker's filtered inventory and runveil's host inventory need not share indexes. HGML quota queries use the same UUID order to locate quota and memory-ledger slots instead of treating host HGML indexes as compute ordinals.

The embedded `gpuworks/partition-probe:v4` verifies both the selected UUID and its memory quota. Missing, invalid, duplicate or unqueryable identities fail, as does a quota applied only to another card. Recreate existing workloads after upgrading the agent to activate the new selection and slicing libraries. Device visibility under runveil still does not provide kernel device isolation.


## Hygon Z100 dGPU

Z100 uses the `hygon-dgpu` backend, assigning CUs in proportion to the memory quota. Build and install the optional `gpuworks-hygon-vdev-helper` on the node (see `build/runtime-assets/hygon-vdev-helper/README.md` in the source tree), and provide the host DTK, HYHAL and DCGM libraries. The agent itself remains a static binary. Only x86_64 + Docker is supported; other models, including K100, do not receive this capability.

The per-GPU probe must confirm that HIP reports the requested memory quota, permits a 1 MiB allocation, and rejects an allocation exceeding the quota by 1 MiB. dGPU scheduling requires a passing probe. Inference containers use their own DTK; host DTK and supplementary ABI libraries are used only by the small probe. The built-in Qwen3-0.6B `bf16-vllm-hygon-z100` spec uses the SourceFind image tested on Z100.

Under a host lock, the helper creates a vdev and saves a configuration snapshot and owner token, preventing stale requests from releasing a later gpuworks allocation that reuses the same ID. The vdev is released only after its container has been removed. Failed cleanup retains the record and the instance stays `removing`; no replacement container starts for that instance. After an agent restart, orphaned records require manual inspection and are not scanned automatically. Other vdev managers that bypass the helper lock and tokens are outside this ownership protection.

## GPU Models and Architectures

When the server receives a node report, it normalizes model names before storing them (`accelerator.NormalizeGPUName`): it removes a leading vendor word
(`NVIDIA`, `Ascend`, `Huawei`, `Hygon`, `AMD`, or `Apple`, followed by whitespace), trims leading and trailing whitespace, and collapses internal whitespace.
Both `NVIDIA GeForce RTX 4090` and `GeForce RTX 4090` are stored as `GeForce RTX 4090`; `Ascend 910B4` is stored as `910B4`.
The inference catalog's name predicates, PD's same-model check, and the GPU list all read this value. The original name is retained only on the agent side (`doctor`, logs).
Existing benchmark snapshots retain the names recorded at the time.

Only raw architecture facts are stored: NVIDIA stores compute capability; other vendors store the most specific identity that can be determined in `arch_id`.
The architecture family (the API's `arch_family`) and NVIDIA's `sm_XY` (the API's `arch_id`) are calculated by `accelerator.GPUArch` at read time
and are not stored in the database. The server does not read the `arch_family` field reported by the agent.

| Vendor | `arch_id` (Stored) | Architecture Family (Derived, Lowercase) |
|---|---|---|
| NVIDIA | Not stored (`sm_XY` is derived from compute capability) | `maxwell` 5.x, `pascal` 6.x, `volta` 7.0/7.2, `turing` 7.5, `ampere` 8.0–8.7, `ada` 8.9, `hopper` 9.x, `blackwell` 10.x–12.x |
| Ascend | Chip model: `910B4`, `910B2C`, `910C`, `310P3` | `910b`, `910c`, `310p`; empty for first-generation 910 and unrecognized models |
| Hygon | SKU: `K100AI`, `BW1000`; stores the series `K100` when only the series is reported; empty if unrecognized | `k100`, `z100`, `bw1000`, `bw1100` |
| PPU | `ZW810E` (from the name) | `beethoven` (the architecture column in `ppu-smi`) |
| Apple | SoC name: `M1 Max`, `M4 Pro` (the model from `ioreg` with `Apple` removed) | Chip generation `m1`…`m6` |

When the agent detects no accelerator cards (none of the vendor CLIs respond or are installed, and no vendor device files exist), the node's vendor is empty; it no longer reports NVIDIA by default.
