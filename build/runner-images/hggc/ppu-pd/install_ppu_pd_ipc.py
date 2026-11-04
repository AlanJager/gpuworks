"""Install PPU same-host PD support into the image's SGLang (opt-in by SGLANG_PPU_PD_IPC=1).

Mooncake's NVLink transport with legacy IPC handles (MC_FORCE_MNNVL=1,
MC_USE_NVLINK_IPC=1) carries the KV between two PPU cards of one host. Two
changes make SGLang work with it, both inert unless SGLANG_PPU_PD_IPC=1:

- the KV, aux and state buffers are registered by the base and size of the
  allocation that holds them (cuMemGetAddressRange), since an IPC handle names
  a whole allocation and the transport cannot find a sub-range otherwise;
- the metadata buffers live on the card, so the transport moves them too;
- with SGLANG_PPU_PD_SERIAL_TRANSFER=1 (and --disable-overlap-schedule) the
  prefill loop never issues an IPC copy while a forward pass runs, which can
  deadlock the PPU driver, and only the last chunk of a chunked prefill is sent.

Every edit is anchored on exact source text and the script fails rather than
guess when the text is not there.
"""
import shutil
import sys
from pathlib import Path

import sglang

ROOT = Path(sglang.__file__).resolve().parent / "srt" / "disaggregation"
HERE = Path(__file__).resolve().parent


def edit(path, old, new):
    text = path.read_text()
    if text.count(old) != 1:
        sys.exit(f"{path}: anchor found {text.count(old)} times, want 1")
    path.write_text(text.replace(old, new))


conn = ROOT / "mooncake" / "conn.py"
edit(conn, """    def register_buffer_to_engine(self):
        # Batch register KV data buffers
""", """    def register_buffer_to_engine(self):
        if os.environ.get("SGLANG_PPU_PD_IPC") == "1":
            from sglang.srt.disaggregation.ppu_ipc_registration import normalize_registration_regions

            regions = list(zip(self.kv_args.kv_data_ptrs or [], self.kv_args.kv_data_lens or []))
            regions += list(zip(self.kv_args.aux_data_ptrs or [], self.kv_args.aux_data_lens or []))
            for ptrs, lens in zip(self.kv_args.state_data_ptrs, self.kv_args.state_data_lens):
                regions += list(zip(ptrs or [], lens or []))
            regions = normalize_registration_regions(regions)
            if regions:
                self.engine.batch_register([p for p, _ in regions], [n for _, n in regions])
            return
        # Batch register KV data buffers
""")

utils = ROOT / "utils.py"
edit(utils, """        elif self.custom_mem_pool:
            # TODO(shangming): Fix me (use 'cuda') when nvlink_transport of Mooncake is bug-free
            device = "cpu"
""", """        elif os.environ.get("SGLANG_PPU_PD_IPC") == "1":
            device = "cuda"
        elif self.custom_mem_pool:
            # TODO(shangming): Fix me (use 'cuda') when nvlink_transport of Mooncake is bug-free
            device = "cpu"
""")

prefill = ROOT / "prefill.py"
edit(prefill, """import hashlib
import logging
""", """import hashlib
import logging

from sglang.srt.disaggregation.ppu_serial_transfer import (
    after_compute as ppu_after_compute,
    enabled as ppu_serial_transfer_enabled,
    finish_transfers as ppu_finish_transfers,
)
""")
edit(prefill, """                result = self.run_batch(batch)
                self.process_batch_result(batch, result)
            else:
                self.on_idle()

            self.process_disagg_prefill_inflight_queue()
""", """                result = self.run_batch(batch)
                if ppu_serial_transfer_enabled():
                    ppu_after_compute(batch, self.attn_tp_cpu_group)
                self.process_batch_result(batch, result)
            else:
                self.on_idle()

            self.process_disagg_prefill_inflight_queue()
            if batch is not None and ppu_serial_transfer_enabled():
                ppu_finish_transfers(self)
""")
edit(prefill, """        \"\"\"
        Send a prefilled chunk to the decode server
        \"\"\"
""", """        \"\"\"
        Send a prefilled chunk to the decode server
        \"\"\"
        # Serialized PPU transfers send the whole KV with the last chunk.
        if ppu_serial_transfer_enabled() and not last_chunk:
            return
""")

for name in ("ppu_ipc_registration.py", "ppu_serial_transfer.py"):
    shutil.copyfile(HERE / name, ROOT / name)
for path in (conn, utils, prefill, ROOT / "ppu_ipc_registration.py", ROOT / "ppu_serial_transfer.py"):
    compile(path.read_text(), str(path), "exec")
print("installed PPU same-host PD support into", ROOT)
