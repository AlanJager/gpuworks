# Third-party license texts

GPUWorks itself is licensed under the Apache License 2.0 (`LICENSE` at the
top level). The components below are bundled as separate programs or
libraries; [`NOTICE`](../NOTICE) lists each one with its upstream, version and
where its source and patches live. This directory carries the license texts
those components require to accompany a binary distribution, and every
release archive ships it next to `LICENSE` and `NOTICE`.

| File | Applies to |
|---|---|
| `GPL-2.0.txt` | PRoot (GPL-2.0-or-later); perftest and rdma-core when used under their GPL option |
| `LGPL-3.0.txt`, `GPL-3.0.txt` | talloc (LGPL-3.0-or-later), statically linked into PRoot; the LGPL incorporates the GPL-3.0 text |
| `MulanPSL-2.0.txt` | ubs-virt (`libvruntime.so`) |
| `perftest-COPYING.txt` | perftest (`ib_write_bw`): GPL-2.0 or OpenIB.org BSD |
| `rdma-core/` | rdma-core, statically linked into `ib_write_bw`: the upstream `COPYING.md` and the license files it refers to |
| `HAMi-core-NOTICE.txt` | HAMi-core (`libvgpu.so`): the upstream NOTICE, propagated as Apache-2.0 section 4(d) requires |

HAMi-core, udocker and the gpustack-operator PPU slicing shim are licensed
under the Apache License 2.0, whose text is the top-level `LICENSE`.
