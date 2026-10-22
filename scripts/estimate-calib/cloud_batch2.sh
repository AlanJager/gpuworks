#!/usr/bin/env bash
# Full A100 sweep, re-run with a runner that tears down vLLM's EngineCore child.
# The first attempt lost every run after the first to an orphaned EngineCore
# holding the device.
set -u
cd /root/calib

M=/root/calib/models

run() { # tag model util maxlen [extra...]
  local tag="$1"; shift
  echo "### $tag  $(date +%T)"
  RUN_TIMEOUT=1200 ./cloud_run2.sh "$tag" "$@" >/dev/null
}

# Direct comparison points against gpu3 (same model, same settings).
run a100-q3-0.6b-u90-L4k    $M/Qwen_Qwen3-0.6B-default 0.9 4096
run a100-q25-7b-u90-L4k     $M/Qwen2.5-7B-Instruct     0.9 4096
run a100-q3-8b-u90-L4k      $M/Qwen3-8B                0.9 4096

# The batch-size axis: 2048 is vLLM's own default for `vllm serve` on this card.
run a100-q25-7b-u90-L4k-mnbt8k  $M/Qwen2.5-7B-Instruct 0.9 4096 --max-num-batched-tokens 8192
run a100-q25-7b-u90-L4k-mnbt16k $M/Qwen2.5-7B-Instruct 0.9 4096 --max-num-batched-tokens 16384

# Architectures gpu3 cannot hold or that exercise the odd config paths.
run a100-gemma3-4b-u90-L4k  $M/gemma-3-4b-it          0.9 4096
run a100-gptoss-20b-u90-L4k $M/gpt-oss-20b            0.9 4096
run a100-dsv2lite-u90-L4k   $M/DeepSeek-V2-Lite-Chat  0.9 4096
run a100-q3-30b-a3b-u90-L4k $M/Qwen3-30B-A3B-Instruct-2507 0.9 4096

echo "### done $(date +%T)"
