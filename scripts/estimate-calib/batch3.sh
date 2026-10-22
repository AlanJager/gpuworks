#!/usr/bin/env bash
# Everything still outstanding on gpu3 after a runner killed mid-poll left a
# container holding device 0 and failed every later run in the first sweep.
# Failures are re-run by deleting their logs first, so the parser cannot mistake
# a startup error for a measurement.
set -u
cd /tmp/calib

M=/models/modelscope

run() { # tag model gpu util maxlen [extra...]
  local tag="$1"; shift
  if [ -s "logs/$tag.log" ] && grep -q "GPU KV cache size" "logs/$tag.log"; then
    echo "skip (measured): $tag"; return
  fi
  echo "### $tag  $(date +%T)"
  RUN_TIMEOUT=900 ./calib_run.sh "$tag" "$@" >/dev/null
}

# --- re-runs: failed on an occupied device in sweep 1 ---
run q25-7b-u90-L4k-eager    $M/Qwen2.5-7B-Instruct     0 0.9 4096 --enforce-eager
run q25-7b-u90-L4k-mnbt2k   $M/Qwen2.5-7B-Instruct     0 0.9 4096 --max-num-batched-tokens 2048
run q3-8b-u90-L4k           $M/Qwen3-8B                0 0.9 4096
run q3-8b-u90-L4k-eager     $M/Qwen3-8B                0 0.9 4096 --enforce-eager
run q3-8b-u70-L4k           $M/Qwen3-8B                0 0.7 4096
run llama31-8b-u90-L4k      $M/Llama-3.1-8B-Instruct   0 0.9 4096
run dsv2lite-u90-L4k        $M/DeepSeek-V2-Lite-Chat   0 0.9 4096
run gemma3-4b-u90-L4k       $M/gemma-3-4b-it           0 0.9 4096

# --- architectures sweep 1 never reached ---
run gptoss-20b-u90-L4k      $M/gpt-oss-20b             0 0.9 4096
run gemma3-12b-u90-L4k      $M/gemma-3-12b-it          0 0.9 4096
run mistral-v03-u90-L4k     $M/mistral-7b-instruct-v03 0 0.9 4096

# --- the batch-size axis on models big enough for it to move ---
run q25-7b-u90-L4k-mnbt16k  $M/Qwen2.5-7B-Instruct     0 0.9 4096 --max-num-batched-tokens 16384
run q3-8b-u90-L4k-mnbt16k   $M/Qwen3-8B                0 0.9 4096 --max-num-batched-tokens 16384
run q25-7b-u50-L4k          $M/Qwen2.5-7B-Instruct     0 0.5 4096

# --- debug runs: vLLM's own decomposition of the reserve it holds back ---
export EXTRA_ENV="-e VLLM_LOGGING_LEVEL=DEBUG"
run dbg-q3-0.6b-u90 $M/Qwen_Qwen3-0.6B-default 0 0.9 4096
run dbg-q3-0.6b-u50 $M/Qwen_Qwen3-0.6B-default 0 0.5 4096
unset EXTRA_ENV

echo "### done $(date +%T)"
