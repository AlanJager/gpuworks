#!/bin/sh
# gpuworks Slurm health check (installed by setup.sh, run by slurmd as root).
#
# slurmd runs this program on every compute node when the node registers, on
# `scontrol reconfigure` and every HealthCheckInterval, and waits for it. It
# therefore returns in seconds and never fails by itself: a non-zero status
# is only ever the one the cluster's original health check program returned.
#
#   1. a node without an NVIDIA GPU is left alone;
#   2. while <script dir>/uninstall-agents exists, the node's agent is removed
#      and never installed;
#   3. a node whose agent service runs is left alone;
#   4. otherwise the agent is installed in the background, under a lock, so
#      overlapping runs do one install at a time and later runs retry. The
#      enrollment token is read from <script dir>/token (root only; this file
#      is world readable and has none); a node that cannot read it, as root,
#      installs nothing. The background worker also asks the server whether
#      the token is still active and installs nothing for a revoked one;
#   5. finally the original health check program (first line of
#      <script dir>/chained) runs, when the node is in a state the cluster
#      asked it to run in (the second line of that file, the original
#      HealthCheckNodeState), and its exit status is ours.
#
# The background worker first waits a random part of half the interval, so
# the nodes of a cluster do not all ask the server (and download the agent)
# at once; the part of the check slurmd waits for never waits.
#
# Nothing here runs a program, or writes through a path, that the agent's
# account (RUN_AS, when not root) can replace: that account owns INSTALL_DIR.
#
# Test hooks (never set by Slurm): GWKS_DEV_DIR (default /dev), GWKS_PROC_DIR
# (default /proc), GWKS_LOCK_DIR (default /run, then GWKS_TMP_DIR),
# GWKS_LOG_DIR (default /var/log: the log of a non-root agent's node),
# GWKS_UNIT_PATH (the agent's systemd unit file), GWKS_SPREAD_MAX (the longest
# wait in seconds, instead of half the interval), GWKS_TMP_DIR (default /tmp,
# the last fallback for the log, the lock and temporary files) and
# GWKS_NO_PATH_EXTEND (do not append the usual system directories to PATH).

# slurmd hands over a sane PATH, but append the system directories anyway;
# appending keeps whatever the caller put first.
if [ -z "${GWKS_NO_PATH_EXTEND:-}" ]; then
	PATH="${PATH:+$PATH:}/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/sbin:/usr/local/bin"
fi
export PATH
set -u

# @@PARAMS@@

DEV_DIR=${GWKS_DEV_DIR:-/dev}
PROC_DIR=${GWKS_PROC_DIR:-/proc}
TMP_DIR=${GWKS_TMP_DIR:-/tmp}
UNIT=gpuworks-agent
UNIT_PATH=${GWKS_UNIT_PATH:-/etc/systemd/system/gpuworks-agent.service}
TOKEN_FILE="$SCRIPT_DIR/token"

# have reports whether a command is on PATH.
have() {
	command -v "$1" >/dev/null 2>&1
}

# bounded runs a command for at most $1 seconds when timeout(1) exists, so a
# wedged tool cannot hold the health check (or the install lock) forever.
bounded() {
	secs=$1
	shift
	if have timeout; then
		timeout "$secs" "$@"
	else
		"$@"
	fi
}

# run_chained runs the cluster's original health check program, if any, and
# exits with its status; with none, or one that is gone, or when the node is in
# no state the program was to run in, it exits 0.
run_chained() {
	prog=''
	want=''
	if [ -f "$SCRIPT_DIR/chained" ]; then
		prog=$(sed -n 1p "$SCRIPT_DIR/chained" 2>/dev/null | tr -d '\r')
		want=$(sed -n 2p "$SCRIPT_DIR/chained" 2>/dev/null | tr -d '\r')
	fi
	# Chaining ourselves would recurse until the node runs out of processes.
	if [ -n "$prog" ] && [ "$prog" != "$SCRIPT_DIR/check.sh" ] && [ -f "$prog" ] && [ -x "$prog" ] && state_wanted "$want"; then
		"$prog" "$@"
		exit $?
	fi
	exit 0
}

# state_wanted says whether the node is in a state the cluster's original
# HealthCheckNodeState ($1: a comma separated list of Slurm's flags, as the
# setup script saved it) runs the program in. Setup makes ours run in any
# state, the install above must; the original program keeps its own rule:
#   ANY (or nothing)   every state
#   ALLOC, MIXED       a node Slurm calls allocated, mixed
#   IDLE               an idle node, drained or not
#   NONDRAINED_IDLE    an idle node that is not drained
#   CYCLE              no state of its own; setup keeps it in slurm.conf, so
#                      slurmctld still spreads the runs over the interval
# A node whose state cannot be read (scontrol cannot be asked from here) runs
# the program, as it did before the gpuworks check stood in front of it.
state_wanted() {
	flags=''
	for w in $(printf '%s' "$1" | tr 'a-z,' 'A-Z '); do
		case "$w" in
		ANY) return 0 ;;
		CYCLE) ;;
		*) flags="$flags $w" ;;
		esac
	done
	[ -n "$flags" ] || return 0
	resolve_node
	[ -n "$NODE_INFO" ] || return 0
	st=$(printf '%s\n' "$NODE_INFO" | tr ' ' '\n' | sed -n 's/^State=//p' | head -n 1 | tr 'a-z' 'A-Z')
	[ -n "$st" ] || return 0
	base=$(printf '%s' "${st%%+*}" | tr -cd 'A-Z')
	drain=0
	case "+$st+" in *+DRAIN+* | *+DRAINING+* | *+DRAINED+*) drain=1 ;; esac
	idle=0 alloc=0 mixed=0
	case "$base" in
	IDLE) idle=1 ;;
	DRAINED) idle=1 drain=1 ;;
	ALLOCATED | ALLOC) alloc=1 ;;
	MIXED) mixed=1 ;;
	DRAINING) alloc=1 mixed=1 drain=1 ;;
	esac
	for f in $flags; do
		case "$f" in
		ALLOC) [ "$alloc" = 1 ] && return 0 ;;
		MIXED) [ "$mixed" = 1 ] && return 0 ;;
		IDLE) [ "$idle" = 1 ] && return 0 ;;
		NONDRAINED_IDLE) [ "$idle" = 1 ] && [ "$drain" = 0 ] && return 0 ;;
		esac
	done
	return 1
}

has_nvidia_gpu() {
	[ -e "$DEV_DIR/nvidia0" ] && return 0
	if have nvidia-smi; then
		bounded 20 nvidia-smi -L 2>/dev/null | grep -q '^GPU ' && return 0
	fi
	return 1
}

# pick_log chooses where background work writes. For an agent that is root,
# the install directory (the agent's own log is there too); for any other
# account that owns the install directory, a root-owned one: root appending
# to a path in a directory the agent can write follows a link it planted.
# /tmp is the last resort, for a read-only or not yet mounted target.
pick_log() {
	LOG=''
	if [ "$RUN_AS" = root ]; then
		if mkdir -p "$INSTALL_DIR" 2>/dev/null && ( : >>"$INSTALL_DIR/slurm-check.log" ) 2>/dev/null; then
			LOG="$INSTALL_DIR/slurm-check.log"
		fi
	else
		for d in "${GWKS_LOG_DIR:-/var/log}" "${GWKS_LOCK_DIR:-/run}"; do
			if ( : >>"$d/gpuworks-slurm-check.log" ) 2>/dev/null; then
				LOG="$d/gpuworks-slurm-check.log"
				break
			fi
		done
	fi
	[ -n "$LOG" ] || LOG="$TMP_DIR/gpuworks-slurm-check.log"
	# Keep the log from growing without bound over months of health checks.
	if [ -f "$LOG" ] && [ "$(wc -c <"$LOG" 2>/dev/null || echo 0)" -gt 1048576 ]; then
		mv -f "$LOG" "$LOG.1" 2>/dev/null || true
	fi
}

pick_lock() {
	lock_dir=${GWKS_LOCK_DIR:-/run}
	LOCK="$lock_dir/gpuworks-slurm-check.lock"
	if ! ( : >>"$LOCK" ) 2>/dev/null; then
		LOCK="$TMP_DIR/gpuworks-slurm-check.lock"
	fi
}

log() {
	printf '%s gpuworks-slurm-check: %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*"
}

# find_slurmd reads the running slurmd's command line. KEPT gets the options
# `slurmd -G` needs to see the same configuration (one per line) and NAME_OPT
# the node name given with -N.
find_slurmd() {
	KEPT=''
	NAME_OPT=''
	for d in "$PROC_DIR"/[0-9]*; do
		[ -r "$d/comm" ] || continue
		comm=$(cat "$d/comm" 2>/dev/null)
		[ "$comm" = slurmd ] || continue
		[ -r "$d/cmdline" ] || continue
		lines=$(tr '\0' '\n' <"$d/cmdline" 2>/dev/null) || continue
		old_ifs=$IFS
		IFS='
'
		set -f
		# shellcheck disable=SC2086
		set -- $lines
		set +f
		IFS=$old_ifs
		[ $# -gt 0 ] && shift # argv[0]
		while [ $# -gt 0 ]; do
			case "$1" in
			-Z)
				KEPT="$KEPT$1
"
				;;
			--conf | --conf-server | -f)
				if [ $# -ge 2 ]; then
					KEPT="$KEPT$1
$2
"
					shift
				fi
				;;
			-N)
				if [ $# -ge 2 ]; then
					KEPT="$KEPT$1
$2
"
					NAME_OPT=$2
					shift
				fi
				;;
			--conf=* | --conf-server=* | -f?*)
				KEPT="$KEPT$1
"
				;;
			-N?*)
				KEPT="$KEPT$1
"
				NAME_OPT=${1#-N}
				;;
			esac
			shift
		done
		return 0
	done
	return 0
}

# resolve_node finds the Slurm node name of this host (NODE): the name slurmd
# was started with, else the short or full host name, whichever scontrol knows.
# NODE_INFO is scontrol's line for that node, empty when it could not be asked.
resolve_node() {
	NODE_INFO=''
	find_slurmd
	short=$(hostname -s 2>/dev/null)
	[ -n "$short" ] || short=$(hostname 2>/dev/null)
	full=$(hostname 2>/dev/null)
	NODE=$short
	for cand in "$NAME_OPT" "$short" "$full"; do
		[ -n "$cand" ] || continue
		if have scontrol; then
			info=$(bounded 20 scontrol -o show node "$cand" 2>/dev/null)
			if printf '%s\n' "$info" | grep -Eq "(^|[[:space:]])NodeName=$cand([[:space:]]|\$)"; then
				NODE=$cand
				NODE_INFO=$info
				return 0
			fi
		fi
	done
	return 0
}

# root_tmp makes a temporary file where only root can create or replace one:
# the lock directory, else the temporary one. The name is printed.
root_tmp() {
	mktemp "${GWKS_LOCK_DIR:-/run}/gwks-$1.XXXXXX" 2>/dev/null || mktemp "$TMP_DIR/gwks-$1.XXXXXX"
}

# write_gres_devices records slurmd's card order for an agent that does not
# run as root: it cannot run `slurmd -G` against the cluster's configuration
# itself (a configless slurmd keeps its cache readable by root only). A failed
# run never replaces a file that already holds a good list. The agent owns the
# directory and can plant a link at any name in it, so the list is made in a
# root-owned temporary file and put in place with install(1), which replaces
# the name and never writes through it.
write_gres_devices() {
	have slurmd || return 0
	have install || return 0
	if [ ! -d "$INSTALL_DIR" ]; then
		mkdir -p "$INSTALL_DIR" 2>/dev/null || return 0
		chown "$RUN_AS" "$INSTALL_DIR" 2>/dev/null || true
	fi
	find_slurmd
	old_ifs=$IFS
	IFS='
'
	set -f
	# shellcheck disable=SC2086
	set -- $KEPT
	set +f
	IFS=$old_ifs
	out=$(bounded 20 slurmd -G "$@" 2>&1) || true
	if printf '%s\n' "$out" | grep -q 'Gres Name=gpu'; then
		tmp=$(root_tmp gres) || return 0
		if printf '%s\n' "$out" >"$tmp" 2>/dev/null; then
			install -m 0644 -o "$RUN_AS" "$tmp" "$INSTALL_DIR/slurm-gres-devices" 2>/dev/null || true
		fi
		rm -f "$tmp"
	fi
}

# read_token puts the enrollment token in TOKEN, reading the first line of
# the token file; it fails when there is none to read.
read_token() {
	TOKEN=$(head -n 1 "$TOKEN_FILE" 2>/dev/null | tr -d '\r\n')
	[ -n "$TOKEN" ]
}

# token_active asks the server, a cheap public question, whether TOKEN is
# still the token of a live entry. The token goes to curl on its standard
# input (curl -K -), not its argument list, which every user can read.
token_active() {
	answer=$(printf 'url = "%s/slurm/token-status?token=%s"\n' "$SERVER_URL" "$TOKEN" | curl -fsS --max-time 15 -K - 2>/dev/null)
	case "$answer" in
	*'"status":"active"'*) return 0 ;;
	esac
	log "the enrollment token is not active (the server said: ${answer:-nothing}): not installing"
	return 1
}

# random_below prints a number from 0 to $1 - 1.
random_below() {
	r=''
	if [ -r /dev/urandom ] && have od; then
		r=$(od -An -N4 -tu4 /dev/urandom 2>/dev/null | tr -d ' \n')
	fi
	case "$r" in
	'' | *[!0-9]*)
		r=$(printf '%s %s %s' "$(hostname 2>/dev/null)" "$$" "$(date +%s 2>/dev/null)" | cksum | awk '{ print $1 }')
		;;
	esac
	echo $((r % $1))
}

# spread waits a random time, up to half the interval, so that the nodes a
# cluster boots together (or an administrator reconfigures at once) do not all
# ask the server, and download the agent from it, in the same second.
spread() {
	max=$((INTERVAL / 2))
	if [ -n "${GWKS_SPREAD_MAX:-}" ]; then
		max=$GWKS_SPREAD_MAX
	fi
	[ "$max" -gt 0 ] 2>/dev/null || return 0
	secs=$(random_below $((max + 1)))
	if [ "$secs" -gt 0 ]; then
		log "waiting ${secs}s (of at most ${max}s) before asking the server: the nodes of a cluster do not all ask at once"
		sleep "$secs"
	fi
}

# install_agent is the background worker: the same script re-entered under
# the lock, so the lock is held for exactly as long as the install runs.
install_agent() {
	spread
	# The previous holder may have finished while this run waited its turn.
	if systemctl is-active --quiet "$UNIT" 2>/dev/null; then
		exit 0
	fi
	if ! read_token; then
		log "cannot read the enrollment token $TOKEN_FILE: not installing"
		exit 0
	fi
	if ! token_active; then
		exit 0
	fi
	resolve_node
	node=$NODE
	log "installing the agent for Slurm node $node"
	# The installer goes to a file only root can write, and its own status is
	# looked at: a pipe from curl would report the shell's status alone.
	script=$(root_tmp install) || {
		log "cannot create a temporary file for the installer: not installing"
		exit 0
	}
	trap 'rm -f "$script"' EXIT
	curl -fsSL --max-time 120 "$SERVER_URL/install.sh" -o "$script"
	rc=$?
	if [ "$rc" -ne 0 ] || [ ! -s "$script" ]; then
		log "downloading the installer from $SERVER_URL/install.sh failed (curl status $rc): not installing, the next run tries again"
		exit 1
	fi
	set -- --server "$SERVER_URL" --install-dir "$INSTALL_DIR" --supervisor systemd
	if [ "$RUN_AS" != root ]; then
		set -- "$@" --run-as "$RUN_AS"
	fi
	# The token travels in the environment, under the names the installer
	# reads: an argument list is world readable.
	GPUWORKS_SLURM_ENROLL_TOKEN="$TOKEN" GPUWORKS_SLURM_NODE="$node" sh "$script" "$@"
	rc=$?
	if [ "$rc" -ne 0 ]; then
		log "the installer failed with status $rc: the next run tries again"
		exit "$rc"
	fi
	log "the installer finished"
	exit 0
}

# uninstall_agent removes this node's agent. An agent that is root runs from a
# directory only root can write, and removes itself. For any other account
# that directory is the agent's own, so nothing in it is run by root: the
# service is stopped and removed with systemd, and the files that make the
# agent this cluster's node (what `gpuworks-agent uninstall` removes as well)
# are unlinked, never followed. Downloaded models stay.
uninstall_agent() {
	if [ "$RUN_AS" = root ]; then
		bounded 120 "$INSTALL_DIR/gpuworks-agent" uninstall --state "$INSTALL_DIR" --yes
		return
	fi
	# Another gpuworks agent on this host (a unit for another directory) stays.
	if ! systemctl cat "$UNIT" 2>/dev/null | grep -Fq -- "--state $INSTALL_DIR"; then
		echo "the $UNIT service does not run from $INSTALL_DIR: leaving it alone"
		return
	fi
	bounded 60 systemctl disable --now "$UNIT" || echo "systemctl disable --now $UNIT failed"
	rm -f "$UNIT_PATH"
	systemctl daemon-reload || true
	for f in identity.json gpuworks-agent.pid agent.lock gpuworks-agent.conf slurm-gres-devices; do
		rm -f "$INSTALL_DIR/$f"
	done
	echo "removed the $UNIT service and the agent's identity"
}

# start_install hands the install to a detached, locked worker and returns at
# once. Failures are retried by the next health check run, not looped here.
start_install() {
	if ! have flock; then
		log "flock not found: not installing" >>"$LOG" 2>&1
		return 0
	fi
	pick_lock
	limit=''
	if have timeout; then
		limit='timeout 1800'
	fi
	# shellcheck disable=SC2086
	if have setsid; then
		GWKS_CHECK_ROLE=install-worker setsid flock -n "$LOCK" $limit sh "$0" >>"$LOG" 2>&1 </dev/null &
	else
		GWKS_CHECK_ROLE=install-worker nohup flock -n "$LOCK" $limit sh "$0" >>"$LOG" 2>&1 </dev/null &
	fi
	return 0
}

if [ "${GWKS_CHECK_ROLE:-}" = install-worker ]; then
	install_agent
fi

has_nvidia_gpu || run_chained "$@"

if [ -e "$SCRIPT_DIR/uninstall-agents" ]; then
	if systemctl cat "$UNIT" >/dev/null 2>&1; then
		pick_log
		log "uninstalling the agent" >>"$LOG" 2>&1
		uninstall_agent >>"$LOG" 2>&1 </dev/null
	fi
	run_chained "$@"
fi

# A non-root agent cannot ask slurmd for the card order itself; refresh the
# list on every run (cards and slurm.conf change), whether or not it runs.
if [ "$RUN_AS" != root ]; then
	write_gres_devices
fi

if systemctl is-active --quiet "$UNIT" 2>/dev/null; then
	run_chained "$@"
fi

pick_log
# Readable by root only: on a root-squashed shared directory the node's root
# cannot see it, and nothing here can fix that, so say so, install nothing and
# leave the node alone (a health check never drains).
if ! read_token; then
	log "cannot read the enrollment token $TOKEN_FILE as root (a root-squashed shared directory? run the setup command again, or copy the file to this node with mode 0600): not installing" >>"$LOG" 2>&1
	run_chained "$@"
fi
start_install
run_chained "$@"
