#!/usr/bin/env bash
# SGLang sweep on device 1 (RTX 2080 Ti, 22 GiB).
#
# The default configuration at mem-fraction-static 0.9 does not survive on a
# 22 GiB card: SGLang sizes the KV pool first and only then captures CUDA
# graphs out of the remaining (1 - mem_fraction_static) headroom, which is not
# enough here. So the axis is split: clean KV allocation with graphs disabled,
# then a separate point that keeps the graph term small enough to complete.
set -u
cd /tmp/calib

M=/models/modelscope

run() { # tag model gpu frac ctx [extra...]
  local tag="$1"; shift
  if [ -s "logs-sglang/$tag.log" ]; then echo "skip (exists): $tag"; return; fi
  echo "### $tag  $(date +%T)"
  RUN_TIMEOUT=600 ./run_sglang.sh "$tag" "$@" >/dev/null
}

# --- KV pool without the graph term ---
run sg-q3-0.6b-f90-L4k-nograph   $M/Qwen_Qwen3-0.6B-default 1 0.9 4096 --disable-cuda-graph
run sg-q3-0.6b-f70-L4k-nograph   $M/Qwen_Qwen3-0.6B-default 1 0.7 4096 --disable-cuda-graph
run sg-q3-0.6b-f90-L32k-nograph  $M/Qwen_Qwen3-0.6B-default 1 0.9 32768 --disable-cuda-graph
run sg-q25-7b-f90-L4k-nograph    $M/Qwen2.5-7B-Instruct    1 0.9 4096 --disable-cuda-graph
run sg-gemma3-4b-f90-L4k-nograph $M/gemma-3-4b-it          1 0.9 4096 --disable-cuda-graph
run sg-dsv2lite-f90-L4k-nograph  $M/DeepSeek-V2-Lite-Chat  1 0.9 4096 --disable-cuda-graph

# --- with the graph term, sized to fit the card ---
run sg-q3-0.6b-f70-L4k-g32       $M/Qwen_Qwen3-0.6B-default 1 0.7 4096 --cuda-graph-max-bs 32

echo "### done $(date +%T)"
