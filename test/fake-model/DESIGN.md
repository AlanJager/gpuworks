# Fake model fixture design

This fixture is deliberately a *control-plane/download* fixture, not an
inference model. It keeps the source index's declared `metadata.total_size`
(the checked-in synthetic index is about 8 GiB for Qwen3-4B) while writing
three valid safetensors shards of exactly about 8 MiB each. Metadata fetch, VRAM
estimation, scheduling, and disk reservation therefore exercise realistic
resource requirements without moving gigabytes over the wire.

The generator's concrete transformation is intentionally simple and stable:

- Every tensor name from `weight_map` is retained, sorted, and reassigned in
  round-robin order to the three generated shard names. The index is rewritten
  with that new mapping; it is not a byte-for-byte copy of the source index.
- In each shard the first tensor is a zero-filled `U8` payload sized to consume
  the shard budget. Any additional names are valid zero-sized `BF16` tensors
  (`shape: [0]`, offsets `[0,0]`). This keeps headers parseable while avoiding
  large allocations and does not claim that the fixture has usable model
  weights.
- If a source repository is supplied with `--src`, small non-safetensors files
  are copied, but all safetensors shards are regenerated and the index mapping
  is still rewritten. The source snapshot must therefore be treated as input
  metadata only, never as a promise of inference compatibility.

Tests must never pass this fixture to an engine. Its purpose is to exercise
control-plane/download paths and integrity bookkeeping only. The generator
also emits this rationale as `DESIGN.md` in every output repository.
