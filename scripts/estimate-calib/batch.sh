#!/usr/bin/env bash
# The dummy-loader measurement matrix for the RTX 8000 (device 0, 45 GiB).
# Each row isolates one axis: model architecture, gpu_memory_utilization,
# max_model_len, max_num_batched_tokens, cudagraph on/off.
set -u
cd /tmp/calib

M=/models/modelscope
U=0.9

run() { # tag model gpu util maxlen [extra...]
  local tag="$1"; shift
  if [ -s "logs/$tag.log" ]; then echo "skip (exists): $tag"; return; fi
  echo "### $tag  $(date +%T)"
  RUN_TIMEOUT=420 ./run_one.sh "$tag" "$@" >/dev/null
}

# --- Qwen3-0.6B: the head_dim-override case (hidden 1024/16 heads = 64, real head_dim 128)
run q3-0.6b-u90-L4k       $M/Qwen_Qwen3-0.6B-default      0 0.9 4096
run q3-0.6b-u90-L32k      $M/Qwen_Qwen3-0.6B-default      0 0.9 32768
run q3-0.6b-u50-L4k       $M/Qwen_Qwen3-0.6B-default      0 0.5 4096
run q3-0.6b-u90-L4k-eager $M/Qwen_Qwen3-0.6B-default      0 0.9 4096 --enforce-eager
run q3-0.6b-u90-L4k-mnbt2k  $M/Qwen_Qwen3-0.6B-default    0 0.9 4096 --max-num-batched-tokens 2048
run q3-0.6b-u90-L4k-mnbt16k $M/Qwen_Qwen3-0.6B-default    0 0.9 4096 --max-num-batched-tokens 16384

# --- Qwen2.5-7B-Instruct: GQA with 4 kv heads
run q25-7b-u90-L4k        $M/Qwen2.5-7B-Instruct          0 0.9 4096
run q25-7b-u90-L32k       $M/Qwen2.5-7B-Instruct          0 0.9 32768
run q25-7b-u50-L4k        $M/Qwen2.5-7B-Instruct          0 0.5 4096
run q25-7b-u90-L4k-eager  $M/Qwen2.5-7B-Instruct          0 0.9 4096 --enforce-eager
run q25-7b-u90-L4k-mnbt2k $M/Qwen2.5-7B-Instruct          0 0.9 4096 --max-num-batched-tokens 2048

# --- Qwen3-8B: 36 layers, 8 kv heads
run q3-8b-u90-L4k         $M/Qwen3-8B                     0 0.9 4096
run q3-8b-u90-L4k-eager   $M/Qwen3-8B                     0 0.9 4096 --enforce-eager
run q3-8b-u70-L4k         $M/Qwen3-8B                     0 0.7 4096

# --- Llama-3.1-8B-Instruct: explicit head_dim 128, 8 kv heads
run llama31-8b-u90-L4k    $M/Llama-3.1-8B-Instruct        0 0.9 4096

# --- DeepSeek-V2-Lite-Chat: MLA (kv_lora_rank 512 + qk_rope_head_dim 64)
run dsv2lite-u90-L4k      $M/DeepSeek-V2-Lite-Chat        0 0.9 4096

# --- gemma-3-4b-it: sliding-window hybrid layer_types
run gemma3-4b-u90-L4k     $M/gemma-3-4b-it                0 0.9 4096

echo "### done $(date +%T)"
