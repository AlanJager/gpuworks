#!/usr/bin/env bash
# Mandatory event-loop gate for a built PRoot binary.
#
#   gate_event_loop.sh <proot>
#
# PRoot's event loop is patched (patches/proot-tracee-index.patch and
# patches/proot-fair-adaptive.patch), so every artifact has to prove two
# things before it is allowed out:
#
#   1. unchanged behaviour: exit codes, a signal death, 300 forks and their
#      wait, a compile-and-run, a 64-thread Python and shell job control;
#   2. fairness: in the many-thread harness every rank's main thread keeps
#      making progress.  A starving tracer leaves ranks at (almost) zero
#      iterations, which is what the unpatched loop does.
#
# Needs gcc, python3 and coreutils on the host, and a native kernel of the
# artifact's architecture.  It takes about half a minute.
set -uo pipefail

proot="${1:?usage: gate_event_loop.sh <proot>}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/proot-gate.XXXXXX")"
trap 'rm -rf "$work"' EXIT
failed=0

run() { "$proot" -0 -k 6.8.0 -r / -b /proc:/proc -b /sys:/sys -b /dev:/dev -w / "$@"; }
check_status() {
	local want="$1" name="$2"
	shift 2
	run "$@" >"$work/out" 2>"$work/err"
	local got=$?
	if [ "$got" != "$want" ]; then
		echo "event-loop gate: $name exited $got, want $want" >&2
		sed -n 1,5p "$work/err" >&2
		failed=1
	fi
}
check_output() {
	local want="$1" name="$2"
	shift 2
	if ! run "$@" 2>"$work/err" | grep -qx "$want"; then
		echo "event-loop gate: $name did not print $want" >&2
		sed -n 1,5p "$work/err" >&2
		failed=1
	fi
}

check_status 7 "exit code" sh -c 'exit 7'
# Upstream PRoot reports 255 when the program dies from a signal.
check_status 255 "signal death" sh -c 'kill -TERM $$'
check_output forks-ok "300 forks" sh -c 'for i in $(seq 300); do /bin/true & done; wait; echo forks-ok'
check_output gcc-3 "compile and run" sh -c \
	'd=$(mktemp -d) && echo "int main(){return 3;}" > $d/s.c && gcc $d/s.c -o $d/s.out; $d/s.out; [ $? = 3 ] && echo gcc-3; rm -rf $d'
check_output threads-ok "64 Python threads" python3 -c 'import threading,os
ts=[threading.Thread(target=lambda: [os.stat("/") for _ in range(2000)]) for _ in range(64)]
[t.start() for t in ts]; [t.join() for t in ts]; print("threads-ok")'
check_output wait-ok "job control" sh -c 'sleep 0.2 & sleep 0.2 & wait; ls /proc/self >/dev/null && echo wait-ok'

# Fairness: 4 ranks x 150 threads, 40 of them busy, 8 s of load.
if ! bash "$here/run-stress.sh" "$proot" 4 150 40 8 60 >"$work/stress" 2>"$work/stress.err"; then
	echo "event-loop gate: the many-thread harness did not finish" >&2
	tail -n 5 "$work/stress.err" >&2
	failed=1
else
	summary="$(grep '^SUMMARY ' "$work/stress" || true)"
	echo "$summary"
	for field in reported=4 ranks_under_100_iters=0 capped=0; do
		case "$summary" in
			*"$field"*) ;;
			*) echo "event-loop gate: fairness run is missing $field: $summary" >&2; failed=1 ;;
		esac
	done
fi

if [ "$failed" != 0 ]; then
	echo "event-loop gate: FAILED" >&2
	exit 1
fi
echo "event_loop_gate=pass"
