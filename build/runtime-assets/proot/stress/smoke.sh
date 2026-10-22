#!/usr/bin/env bash
# Behaviour smoke for a PRoot binary: exit codes, fork/wait, a compile-and-run
# and a threaded Python, all with seccomp acceleration on. Run it against the
# pre-fairness and the shipped binary; the output must be identical.
#
#   smoke.sh <proot>
#
# Needs sh, gcc and python3 on the host. PRoot reports 255 when the program is
# killed by a signal (upstream behaviour), so that line is expected to say 255.
set -u
P="${1:?usage: smoke.sh <proot>}"
run() { "$P" -0 -k 6.8.0 -r / -b /proc:/proc -b /sys:/sys -b /dev:/dev -w / "$@"; }

run sh -c 'exit 7'; echo "exit 7 -> $? (want 7)"
run sh -c 'kill -TERM $$'; echo "killed by SIGTERM -> $? (want 255)"
run sh -c 'for i in $(seq 300); do /bin/true & done; wait; echo forks-ok'
run sh -c 'd=$(mktemp -d) && echo "int main(){return 3;}" > $d/s.c && gcc $d/s.c -o $d/s.out && $d/s.out; echo "gcc-run -> $? (want 3)"; rm -rf $d'
run python3 -c 'import threading,os
ts=[threading.Thread(target=lambda: [os.stat("/") for _ in range(2000)]) for _ in range(64)]
[t.start() for t in ts]; [t.join() for t in ts]; print("threads-ok")'
run sh -c 'sleep 0.2 & sleep 0.2 & wait; ls /proc/self >/dev/null && echo wait-ok'
