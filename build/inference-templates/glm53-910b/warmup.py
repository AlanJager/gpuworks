#!/usr/bin/env python3
import cann_ops_transformer as ops

for name in ("key_pool", "pool_key_indexer"):
    if not hasattr(ops, name):
        raise RuntimeError(f"required CANN extension was not loaded: {name}")
print("CANN_WARMUP_OK key_pool pool_key_indexer", flush=True)

from check_weights import check_weights
from check_draft_config import check_draft_config

check_weights()
check_draft_config()
