# Embedded benchmark datasets

Only the OASST1 gzip JSONL file in this directory is embedded as corpus data.
It is a deterministic, size-bounded subset of a real upstream dataset. It is
are useful for repeatable workload shapes, but neither source is captured
production traffic.

`random` and `agentic` modes do not use this corpus. They create fresh random tokenizer
vocabulary samples when a tokenizer is available, or fresh printable
alphanumeric payloads at approximately four characters per token otherwise.

## `multiturn.jsonl.gz`

- Source: OpenAssistant `OpenAssistant/oasst1`, Apache-2.0.
- Revision: `fdf72ae0827c1cda404aff25b6603abec9e3399b`.
- Source file: `2023-04-12_oasst_ready.trees.jsonl.gz`.
- Source URL: <https://huggingface.co/datasets/OpenAssistant/oasst1/blob/fdf72ae0827c1cda404aff25b6603abec9e3399b/2023-04-12_oasst_ready.trees.jsonl.gz>
- Embedded subset: 3,826 conversation paths; 2,499,221 compressed bytes and
  6,925,225 uncompressed bytes.
- Extraction: `scripts/prepare_multiturn_dataset` rejects deleted or
  review-failed nodes, orders replies by upstream rank, chooses one reviewed
  path from each tree, maps roles to OpenAI `user`/`assistant`, and takes the
  largest deterministic prefix that fits the 2,500,000-byte gzip budget.

At runtime, the first real user message starts the conversation and later real
user messages become follow-up turns. Assistant replies are produced by the
endpoint under test rather than replayed from the source.

## Reproduction and license

Run `scripts/prepare_bench_datasets.sh --refresh-upstream` to download the
pinned revision and recreate the asset. Running the script without that flag
is network-free and validates the checked-in gzip files and aggregate size.

The upstream dataset card declares the Apache License 2.0:
<https://www.apache.org/licenses/LICENSE-2.0>. Preserve this provenance and the
pinned revisions whenever the embedded subsets are regenerated or replaced.
