#!/bin/sh
# Print a `go test -run` pattern selecting one shard of a suite's top-level
# tests, so CI can split a long single-package suite across parallel jobs.
#
#   scripts/go_test_shard.sh <index>/<total> <go test -list args...>
#   scripts/go_test_shard.sh 2/3 -tags=multiproc ./test/l2/...
#
# Tests are sorted by name and dealt round-robin (the 1st, 4th, 7th... to shard
# 1 of 3), which spreads a family of slow cases sharing a name prefix across
# shards. Every test lands in exactly one shard, and a shard with no tests
# prints a pattern that matches nothing rather than an empty one, which would
# match everything.
set -eu

shard=${1:?usage: go_test_shard.sh <index>/<total> <go test -list args...>}
shift
index=${shard%/*}
total=${shard#*/}
case "$index$total" in
*[!0-9]* | "") echo "go_test_shard.sh: shard must be <index>/<total>, got $shard" >&2; exit 2 ;;
esac
if [ "$total" -lt 1 ] || [ "$index" -lt 1 ] || [ "$index" -gt "$total" ]; then
	echo "go_test_shard.sh: shard $shard is out of range" >&2
	exit 2
fi

list=$(${GO:-go} test -list '.*' "$@") || {
	printf '%s\n' "$list" >&2
	exit 1
}
names=$(printf '%s\n' "$list" | grep -E '^(Test|Example|Fuzz)[A-Za-z0-9_]*$' | LC_ALL=C sort -u |
	awk -v i="$index" -v n="$total" '(NR - 1) % n == i - 1' | paste -sd '|' -)
if [ -z "$names" ]; then
	echo '^$'
else
	echo "^($names)\$"
fi
