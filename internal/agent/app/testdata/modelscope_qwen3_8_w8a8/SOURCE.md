Metadata from [Eco-Tech/Qwen3.8-27B-w8a8 on ModelScope](https://modelscope.cn/models/Eco-Tech/Qwen3.8-27B-w8a8), revision `ca2937f18b7e399c0ee91c728a4cde9b837aebff`, downloaded 2026-10-30. The source model card declares Apache License 2.0.

The three JSON files are unmodified. Their sizes and SHA-256 were checked against the repository file listing:

| File | Bytes | SHA-256 |
|---|---:|---|
| config.json | 4312 | 191e0af232104ed8b65258cf3fb2b842e288008baca7633c11b82a1ac7203aab |
| configuration.json | 29 | 8fa209ef18ac53dea0c47cf8d6f4e92c907565a4971bee413c1772e314a251fa |
| quant_model_weights.safetensors.index.json | 230367 | 512d41c5403b1fe0a65861ffe70da9a84a1e93a534e132fd31a01fdc32844ae9 |

The index references 2,079 tensors across ten shards. The test creates placeholder shard files: it validates registration and missing-file detection, not tensor decoding or inference. The public revision's ten shard names and sizes match the incident listing; the node's file hashes were not available.

Download a file with the fixed revision through:
`https://modelscope.cn/api/v1/models/Eco-Tech/Qwen3.8-27B-w8a8/repo?Revision=ca2937f18b7e399c0ee91c728a4cde9b837aebff&FilePath=<filename>`.
