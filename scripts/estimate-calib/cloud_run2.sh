#!/usr/bin/env bash
# One vLLM dummy-loader measurement on a rented single-GPU host.
#
# vLLM spawns an EngineCore child that holds the GPU. Killing only the process
# the shell started leaves that child alive, and every later run then dies with
# "Free memory ... is less than desired GPU memory utilization" - which is
# exactly what happened to the first A100 sweep. So teardown runs the process
# group down and then verifies against nvidia-smi that nothing still owns the
# device.
#
#   usage: cloud_run2.sh <tag> <model_dir> <util> <maxlen> [extra vllm args...]
set -u

TAG="$1"; MODEL="$2"; UTIL="$3"; MAXLEN="$4"; shift 4

OUTDIR="${OUTDIR:-/root/calib/logs}"
RUN_TIMEOUT="${RUN_TIMEOUT:-900}"
mkdir -p "$OUTDIR"
LOG="$OUTDIR/$TAG.log"

if [ -s "$LOG" ] && grep -q "GPU KV cache size" "$LOG"; then
  echo "skip (measured): $TAG"; exit 0
fi
rm -f "$LOG"

PORT=$(( 18000 + (RANDOM % 2000) ))

setsid vllm serve "$MODEL" --load-format dummy \
  --gpu-memory-utilization "$UTIL" --max-model-len "$MAXLEN" \
  --port "$PORT" "$@" > "$LOG" 2>&1 &
PID=$!
PGID=$(ps -o pgid= -p "$PID" 2>/dev/null | tr -d ' ')

deadline=$(( $(date +%s) + RUN_TIMEOUT ))
while :; do
  grep -qE "GPU KV cache size|EngineCore failed|ValueError|RuntimeError|Traceback \(most recent" "$LOG" 2>/dev/null && { sleep 3; break; }
  kill -0 "$PID" 2>/dev/null || break
  if [ "$(date +%s)" -ge "$deadline" ]; then echo "TIMEOUT after ${RUN_TIMEOUT}s" >> "$LOG"; break; fi
  sleep 2
done

[ -n "${PGID:-}" ] && kill -9 -"$PGID" 2>/dev/null
kill -9 "$PID" 2>/dev/null
# Whatever is still registered on the device is an orphan of this run; no other
# tenant shares the box.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  pids=$(nvidia-smi --query-compute-apps=pid --format=csv,noheader 2>/dev/null | tr -d ' ')
  [ -z "$pids" ] && break
  for p in $pids; do kill -9 "$p" 2>/dev/null; done
  sleep 2
done
echo "log: $LOG"
