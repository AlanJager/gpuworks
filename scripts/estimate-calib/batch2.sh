#!/usr/bin/env bash
# Second vLLM pass on device 0: the architectures the first sweep did not cover
# (real sliding-window hybrids, a model whose config.json drops head_dim, and
# the batch-size axis on a model big enough for it to matter), plus two
# debug-level runs that print vLLM's own memory-profile decomposition.
set -u
cd /tmp/calib

M=/models/modelscope

run() { # tag model gpu util maxlen [extra...]
  local tag="$1"; shift
  if [ -s "logs/$tag.log" ]; then echo "skip (exists): $tag"; return; fi
  echo "### $tag  $(date +%T)"
  RUN_TIMEOUT=900 ./run_one.sh "$tag" "$@" >/dev/null
}

run gptoss-20b-u90-L4k     $M/gpt-oss-20b             0 0.9 4096
run gemma3-12b-u90-L4k     $M/gemma-3-12b-it          0 0.9 4096
run mistral-v03-u90-L4k    $M/mistral-7b-instruct-v03 0 0.9 4096
run q25-7b-u90-L4k-mnbt16k $M/Qwen2.5-7B-Instruct     0 0.9 4096 --max-num-batched-tokens 16384
run q25-7b-u50-L4k         $M/Qwen2.5-7B-Instruct     0 0.5 4096
run q3-8b-u90-L4k-mnbt16k  $M/Qwen3-8B                0 0.9 4096 --max-num-batched-tokens 16384

# Same model, same settings, only util differs: isolates whatever term in
# vLLM's profile moves with the utilization budget.
export EXTRA_ENV="-e VLLM_LOGGING_LEVEL=DEBUG"
run dbg-q3-0.6b-u90 $M/Qwen_Qwen3-0.6B-default 0 0.9 4096
run dbg-q3-0.6b-u50 $M/Qwen_Qwen3-0.6B-default 0 0.5 4096
unset EXTRA_ENV

echo "### done $(date +%T)"
