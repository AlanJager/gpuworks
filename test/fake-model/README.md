# fake-model generator

Warning: generated repositories are not runnable model weights. They are tiny
structural fixtures for control-plane and download tests; do not deploy them to
vLLM/SGLang or use them for inference.

Generate the offline fixture with:

```sh
go run ./test/fake-model --out testdata/qwen3-4b-fake
```

To use metadata from a local Qwen3 snapshot, pass `--src /path/to/Qwen3-4B`.
The generator copies small auxiliary files, but always regenerates all
safetensors shards, reassigns tensor names in the index, and keeps the index's
declared `total_size` so scheduling tests remain realistic. The resulting
repository is still a test fixture, not an inference-capable copy of the
source model.

See [DESIGN.md](DESIGN.md) for the exact shard and tensor transformation. The
same fixture is served by the L1 fake HuggingFace and ModelScope hubs; it is
never used for inference.

## Public mirrors

The generated fixture has been published for optional network-enabled smoke
tests and for reviewers who want to inspect the repository metadata directly:

- [ModelScope: `zstack/qwen3-4b-fake`](https://www.modelscope.cn/models/zstack/qwen3-4b-fake)
- [Hugging Face: `zsatckai/qwen3-4b-fake`](https://huggingface.co/zsatckai/qwen3-4b-fake)

The public copies are convenience mirrors, not a replacement for the local
`FakeModelHub` fixture. Default tests remain offline and do not download from
either service. The Hugging Face account spelling (`zsatckai`) is intentional
for the published URL; local fixture identifiers may continue to use the
historical `zstackai/qwen3-4b-fake` name.
