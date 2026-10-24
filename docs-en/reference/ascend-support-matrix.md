# Ascend Support Matrix

Updated: 2026-08-28. This document distinguishes three things that are easy to confuse:

- **Outer CANN**: the toolkit provided by the node or rented cloud marketplace image. Self-owned bare-metal machines may have only a driver, recorded as `driver-only`; this does not mean that no container runtime is available.
- **Inner CANN**: the CANN userspace application binary interface (userspace ABI) in the workload image launched by GPUWorks. It affects whether the workload image can run, but does not participate in `libvruntime.so` asset selection.
- **Runtime (container runtime)**: Docker, nerdctl, or runveil (`udocker` driver). Gitee rental images provide only the outer environment, not the workload image; the `udocker` scenarios in the table use PRoot to run workload containers inside the outer container (container-in-container). Results marked `udocker` below were verified with the old udocker CLI launcher and have not yet been retested on Ascend with runveil.

## Current Tested Matrix

| Environment | Architecture / NPU | Outer CANN | Inner CANN / Runtime | Highest Level Passed | Conclusion |
|---|---|---:|---|---|---|
| Gitee x86_64 | x86_64 / 910B2C | 9.0 | 9.0 / udocker | Platform full-GPU deployment + `/v1/chat/completions` | Supported; test explicitly asserts `runtime_driver=udocker` |
| Gitee x86_64 | x86_64 / 910B2C | 8.5.1 | 8.5 / udocker | Platform deployment reaches healthy; dGPU capability probe correctly reports cloud-side denial | Outer 8.5 + inner 8.5 can start; successful chat evidence reproducible in CI is still missing |
| Gitee ARM | arm64 / 910B1 | 9.0 | 9.0 / udocker | Platform full-GPU deployment + `/v1/chat/completions` | Supported; actually prefetches arm64 manifest/layers |
| Gitee ARM | arm64 / 910B1 | 9.0 | 8.5 / udocker | Container starts as far as CANN FE | Unsupported; realpath of `aarch64-linux/lib64/../data/platform_config` fails at the PRoot boundary |
| Gitee ARM catalog entry | arm64 / 910B1 | 8.5 | Inner environment not reached | Rental prerequisite validation | The cloud marketplace actually returns “image and host do not match,” so this cannot serve as a CI fixture; this does not establish incompatibility between outer 8.5 and inner 8.5 |
| Self-owned pool Docker member | arm64 / 910B4 | 9.0/driver | 9.0 / Docker | Platform API dGPU with two replicas on one card, real inference/chat, overcommit rejection, full-GPU chat | Supported; complete primary acceptance for platform dGPU |
| Self-owned pool nerdctl member | arm64 / 910B4 | driver-only | Embedded probe / nerdctl | Production path loads a 2.5 MiB behavioral probe; `dsmi_get_hbm_info` returns exactly 4 GiB | Node-level dGPU supported; no workload image, model, or inference engine |

“Outer 8.5 and inner 8.5 have been tested” refers to the Gitee x86_64 environment in the table: there is a record of a real platform deployment reaching healthy, but the current repository does not retain a successful chat response, so the matrix does not mark complete chat as passed. The next step is to reuse the same instance in the existing x86_64/CANN8.5 rental test case to add chat verification, avoiding another rental.

## Coverage Levels

1. **Detection**: architecture, NPU, driver/toolkit, runtime, and the reason for dGPU capability status.
2. **Device injection**: the specified physical NPU is visible inside the inner container.
3. **dGPU behavioral probe**: use the production `Engine` to generate the vCANN configuration and start the real runtime; the embedded 2.5 MiB `/probe` calls `dsmi_get_hbm_info` and asserts that the visible total GPU memory is exactly 4 GiB. This level deliberately does not load a workload image or model.
4. **Platform dGPU inference**: create deployments and instances through the GPUWorks API and verify multiple replicas on one card, quotas, and real inference.
5. **Complete full-GPU chat**: deploy through the platform API and receive a real response from `/v1/chat/completions`.

The self-owned pool covers levels 1–5. Because Gitee rental containers cannot run `npu-smi set -t device-share`, they cover only detection and full-GPU container-in-container chat. Cloud-side permission denial must not be presented as a dGPU pass.

## Two Corrected Misdiagnoses

### “No Runtime”

The historical ARM fixture reported “no available docker/udocker runtime on the node.” This was neither a CANN declaration nor a hardware incompatibility. Python in that outer image is at `/root/miniconda3/bin/python3` or `/opt/conda/bin/python3`, outside the default `PATH` of a non-interactive shell, so the old detection logic did not enable embedded udocker/PRoot.

Detection now checks these absolute paths and correctly identifies the runtime as `udocker`. This detection is now relevant only for `GPUWORKS_UDOCKER_LAUNCHER=udocker`: the default runveil launcher does not require Python.

### `libvruntime.so` Is Distinguished Only by CPU Architecture

Historically, artifacts for the same architecture in the CANN 8.5.1 and 9.0.0 directories were byte-for-byte identical: the arm64 SHA256 was `3dbe727a…d3d49` for both, and the amd64 SHA256 was `57a9641a…9d010` for both. They came from the same pinned ubs-virt source and patch, not two separate runtime ABIs.

The repository now retains one copy each of `linux-arm64/libvruntime.so` and `linux-amd64/libvruntime.so` as the sole canonical path for each architecture. At runtime, only these two paths are accepted; neither outer nor inner CANN versions are read, and there is no fallback across CANN versions.

After the agent upgrades and successfully installs assets at the canonical paths, it deletes precisely the old `cann-8.5.1/` and `cann-9.0.0/` state directories. CANN versions remain only as records of tested environments and compatibility warnings.

A passing node behavioral probe proves only that the “node + runtime + this SO (shared library)” dGPU path works; it does not automatically prove that any workload image works. Workload image support must be determined by a dedicated image-specific behavioral probe using that image's userspace environment. Until this probe is completed, the existing version matrix can only be marked warning/unknown and cannot establish support. The PRoot `platform_config` failure on ARM with outer 9 + inner 8.5 demonstrates why these two conclusions must remain separate.

`PartitionProfile.Verified` is a verification flag in the code and cannot replace the real-hardware matrix above:

- **Definition**: the profiles table in `internal/accelerator/ascend.go`, a static Boolean hardcoded by backend/framework/version prefix/architecture.
- **Meaning**: only indicates that the combination is marked in the code as verified in a preset real-hardware environment. When false, `partitioncap` appends “This runtime combination has not yet been verified in a preset real-hardware environment.”
- **Maintenance**: maintained manually with code commits; it does not automatically synchronize with this document's real-hardware matrix. For example, CANN 9.0.x × x86_64 has a complete chat record, but this field is not yet marked Verified.
- **Validation boundary**: does not indicate additional build/provenance validation of the corresponding `libvruntime.so` asset itself; that validation is performed independently by `build/runtime-assets/verify.sh`.

Use the real-hardware matrix above as the basis for support conclusions.

## CI Ownership

- `l4-gitee-ascend-partition-capability`: rents an x86_64/CANN8.5 instance to verify node capability detection and known device-share restrictions, without downloading workload models.
- `l4-gitee-ascend-deploy-chat`: explicitly runs full-GPU deployments and real chat on x86_64/CANN9 and arm64/CANN9 at low frequency.
- `l4-ascend-pool-partition-inference`: treats `.31/.32` as a capability pool rather than fixed nodes; verifies platform API dGPU inference/full-GPU chat, nerdctl device injection, and the embedded behavioral probe.
- The nerdctl helper is an approximately 9 MiB gzipped arm64 asset in the repository. CI and GPU nodes do not run `go build`; when updating it, build only on the internal build machine (see `secrets/test-environments.md`) and then check it in.

For failure debugging, preserve the last environment by default: use `GPUWORKS_CLOUD_KEEP_ON_FAILURE=1` for cloud rentals and `GPUWORKS_KEEP_ON_FAILURE=1` for the nerdctl helper. After identifying the cause, clean up the exact instances, containers, and temporary directories from that run.

## Cards and Chips: Identification and Numbering

Each chip is an independent device with its own `/dev/davinciN`, PCIe function, HBM, and health status. GPUWorks reports and schedules by chip.
Card layout is determined from the `npu-smi info` table, consistent with zstack-utility's `resolve_handler`:

- **One chip per card (910B)**: each NPU has one chip row, and the header has no `Phy-ID` column. The device number is the NPU ID.
- **Multiple chips per card (910C)**: the header has a `Phy-ID` column, multiple Chip IDs appear under the same NPU, or the chip name contains `910C`.
  The 910C chip name column shows `Ascend910` (without a suffix), with two chips per NPU (Chip 0/1); GPUWorks records these chips as model `910C`.
  The first-generation 910 is a single-chip card with the same name and is not identified as 910C. The device number is the chip's Phy-ID; chips without a Phy-ID cannot be mapped to a device and are not reported.

Basis for using Phy-ID consistently as the device number (read-only verification on two 910C hosts on 2026-10-02; records at
`artifacts/archive/2026-10-02-gpu-identity/evidence/`):

| Use | Numbering |
|---|---|
| `/dev/davinciN`, `ASCEND_VISIBLE_DEVICES` (Ascend Docker Runtime), nerdctl `--device` | Phy-ID |
| `npu-smi info -t topo` matrix | `Phy-ID<n>` rows and columns (`SIO` between chips and `HCCS_SW` between cards; both treated as high-speed interconnects) |
| `hccn_tool -i` | Phy-ID (zstack-utility `fedf490a2` also switches to physical IDs) |
| `npu-smi info -t health/board/device-share -i <npu> -c <chip>` | NPU ID + Chip ID, converted from the table by the agent |
| `ASCEND_RT_VISIBLE_DEVICES` inside the container | Compact numbering starting at 0 |

For 910C, Logic ID in `npu-smi info -m` equals Phy-ID and is not compacted when cards are missing (on a host missing NPUs 3–6, NPU 7's two chips remain 14 and 15).
All `/dev/davinci0..15` devices exist and map one-to-one to Phy-ID (`Phy-ID = 2 × NPU ID + Chip ID`).
For 910B, NPU ID equals Phy-ID, so single-chip cards continue to use NPU ID.
