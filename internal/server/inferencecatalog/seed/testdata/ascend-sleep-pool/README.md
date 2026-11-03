# Fixtures: vllm-ascend sources the sleep-pool patch is pinned to

`vllm_ascend/worker/worker.py` and `vllm_ascend/worker/model_runner_v1.py` are
the verbatim upstream sources of vllm-ascend `0.1.dev5+g550ab66e7`
(2026-09-11), extracted from the image the Qwen3.8-Flash-Next 910C (A3) recipe
pins:

    quay.io/ascend/vllm-ascend@sha256:2cbb274af050cb5be7db7318e1c461fe22ec0643c54f1ed32aa7266513a7d421

They are Apache-2.0 (see `LICENSE.txt`). The catalog patch digest-pins these
exact bytes, so the files must stay unmodified: `harness.py` reports the pool
scope before and after patching, which is what
`ascend_sleep_pool_patch_test.go` asserts.
