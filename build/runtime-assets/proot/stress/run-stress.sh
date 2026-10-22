#!/usr/bin/env bash
# Many-thread starvation stress for a PRoot binary.
#
#   run-stress.sh <proot> [ranks=8] [threads_per_rank=300] [busy_per_rank=60] [run_seconds=15] [cap_seconds=120]
#
# Runs threadstorm.c (static, built on the fly with gcc) under <proot> in the
# same mode runveil uses (-0 -k 6.8.0 -r /, seccomp acceleration on) and prints
# one line per rank plus a summary.  A starving tracer shows up as slow thread
# start-up (setup_ms), ranks whose main thread makes (almost) no progress
# (main_iters) and a max_gap_ms close to the whole run; a fair one keeps every
# main thread moving.  A rank that hits cap_seconds is reported as capped.
#
# Passing this stress is necessary, not sufficient: a candidate event-loop change
# must also survive a real many-thread load (see README.md).
set -euo pipefail

proot="${1:?usage: run-stress.sh <proot> [ranks] [threads] [busy] [run_seconds] [cap_seconds]}"
ranks="${2:-8}"
threads="${3:-300}"
busy="${4:-60}"
run="${5:-15}"
cap="${6:-120}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/proot-stress.XXXXXX")"
cleanup() {
	# PRoot is not stopped by SIGKILL of the tracer alone: its tracees would
	# keep running, so remove them by the unique binary path as well.
	pkill -KILL -f "$work/threadstorm" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

gcc -static -O2 -pthread "$here/threadstorm.c" -o "$work/threadstorm"

# PRoot ignores SIGTERM, so the outer timeout has to be SIGKILL.
timeout -s KILL "$((cap + 90))" "$proot" --kill-on-exit -0 -k 6.8.0 -r / \
	-b /proc:/proc -b /sys:/sys -b /dev:/dev -w / \
	"$work/threadstorm" "$ranks" "$threads" "$busy" "$run" "$cap" | tee "$work/out"

awk -v ranks="$ranks" -v threads="$threads" '
function val(field,   a) { split(field, a, "="); return a[2] + 0 }
/^rank=/ {
	n++;
	setup = val($2); runms = val($3); iters = val($4); gap = val($5); cap += val($6);
	if (setup > setup_max) setup_max = setup;
	setup_sum += setup;
	if (n == 1 || iters < iters_min) iters_min = iters;
	iters_sum += iters; run_sum += runms;
	if (gap > gap_max) gap_max = gap;
	if (iters < 100) starved++;
}
END {
	printf "SUMMARY ranks=%d total_threads=%d reported=%d setup_ms_max=%d setup_ms_avg=%.0f main_iters_min=%d main_iters_per_s_avg=%.0f max_gap_ms=%d ranks_under_100_iters=%d capped=%d\n",
		ranks, ranks * threads, n, setup_max, setup_sum / (n ? n : 1), iters_min,
		(run_sum > 0 ? iters_sum / (run_sum / 1000) : 0), gap_max, starved + 0, cap;
}' "$work/out"
