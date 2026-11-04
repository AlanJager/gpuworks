"""Run PPU IPC KV copies only while the prefill engine computes nothing.

On a PPU host, a legacy-IPC copy issued while a forward pass runs on the same
card can deadlock in the driver. With SGLANG_PPU_PD_SERIAL_TRANSFER=1 the
prefill loop waits for the forward to finish before handing KV to the
transport, and drains every transfer it started before the next forward.

Only real transfers are drained. A health check or warmup request carries the
fake bootstrap host: its sender copies nothing and may stay in the queue until
a later poll, so waiting for it would stall the scheduler (the engine's own
health check then times out and the instance is restarted).
"""
import logging
import os
import time

import torch
import torch.distributed as distributed

LOGGER = logging.getLogger(__name__)

DRAIN_TIMEOUT_SECONDS = 60


def enabled():
    return os.environ.get("SGLANG_PPU_PD_SERIAL_TRANSFER") == "1"


def after_compute(batch, cpu_group):
    torch.cuda.synchronize(batch.seq_lens.device)
    distributed.barrier(group=cpu_group)


def _real_transfers(queue):
    from sglang.srt.disaggregation.utils import FAKE_BOOTSTRAP_HOST

    return [req for req in queue if getattr(req, "bootstrap_host", None) != FAKE_BOOTSTRAP_HOST]


def finish_transfers(scheduler):
    started = time.monotonic()
    initial = len(_real_transfers(scheduler.disagg_prefill_inflight_queue))
    while _real_transfers(scheduler.disagg_prefill_inflight_queue):
        scheduler.process_disagg_prefill_inflight_queue()
        if time.monotonic() - started > DRAIN_TIMEOUT_SECONDS:
            # A transfer whose decoder never came stays in the queue until it
            # is aborted; the scheduler must keep serving rather than die.
            LOGGER.warning("PPU IPC transfers still in flight after %ss; continuing", DRAIN_TIMEOUT_SECONDS)
            break
        time.sleep(0.001)
    distributed.barrier(group=scheduler.attn_tp_cpu_group)
    if initial:
        LOGGER.info("PPU serialized IPC drained %s request(s) in %.3f seconds", initial, time.monotonic() - started)
