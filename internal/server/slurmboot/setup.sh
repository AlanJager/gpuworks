#!/bin/sh
# gpuworks Slurm setup: run once as root on the Slurm controller.
#
#   setup.sh [--interval N] [--no-node-reach]
#                               install the health check (idempotent)
#   setup.sh --check [--interval N] [--no-node-reach]
#                               say what the install would do and change nothing
#   setup.sh --remove           take the health check out again
#   setup.sh --uninstall-agents arm the "uninstall" mode of the health check
#
# Install: writes <script dir>/check.sh, copies it to the compute nodes when
# the script directory is not shared storage, adds one block to slurm.conf
# that makes it the HealthCheckProgram (the cluster's own program, if any, is
# chained by check.sh and keeps its exit status), then runs
# `scontrol reconfigure`. slurmd then runs the check on every node, at
# registration and every HealthCheckInterval, and the check installs the agent
# on GPU nodes, including nodes added later.
#
# Both also ask whether the server's address ($SERVER_URL/healthz) can be
# reached from this controller and from each GPU node (read-only curl, over
# clush, pdsh or ssh as the copy to the nodes goes); --no-node-reach asks the
# controller only, for a caller that asks the nodes some other way. An
# address a host cannot reach makes the run partial.
#
# Uninstalling the agents is two steps: --uninstall-agents makes every node's
# check remove its agent on its next run; after one HealthCheckInterval,
# --remove restores slurm.conf.
#
# --check changes nothing on the cluster: it writes no file, copies nothing,
# leaves slurm.conf alone and does not reconfigure; the only thing it creates
# is the temporary directory the precheck makes, which it removes. It prints
# the install's own markers for the steps whose facts it knows, with
# status=plan where the install prints status=ok, plus:
#   step=install_mode status=plan rearm=yes|no  (an earlier uninstall is re-armed)
#   step=distribute_plan nodes=<comma list or none> files=<comma list> [skipped=shared]
#   step=conf_comment line=<n> key=<HealthCheck key> value=<value>  (one per
#                 setting that would be commented in place)
#   step=slurm_conf status=plan path=<file> chained=<program or none>
#                 interval=<n> node_state=<s> changed=yes|no
#   step=reconfigure status=plan
#   step=done status=ok|partial mode=check
#
# Machine-readable output: next to the human text, every step prints one line
#   GWKS_SLURM key=value key=value ...
# Values contain no spaces (anything odd is replaced by "_"); node lists are
# comma separated. The lines, in the order they appear:
#   step=precheck status=ok cluster=<ClusterName>
#   step=precheck status=failed reason=<not_root|scontrol_not_found|
#                 slurmctld_unreachable|no_cluster_name|conf_unreadable|
#                 not_controller_conf|not_slurmctld_host|script_dir_unsafe|
#                 bad_arguments|conf_unterminated_block>
#   step=script_dir status=ok|plan|failed path=<dir> [action=tighten|tightened]
#                 [owner=<uid> mode=<octal>]   (setup and check modes; failed
#                 names the directory on the script directory's path that
#                 others could write, before precheck failed)
#   step=conf_source status=warn reason=program_not_in_conf program=<path>
#                 (slurmctld runs a health check program slurm.conf does not
#                 name: an included file, or an edit not yet reloaded)
#   step=conf_include status=warn files=<comma list>   (slurm.conf has Include)
#   step=check_script status=ok|failed path=<script dir>/check.sh
#   step=token_file status=ok|failed path=<script dir>/token   (install only;
#                 the token is written root-only and read back)
#   step=chained status=ok program=<path or none> node_state=<the original
#                 HealthCheckNodeState, e.g. ANY or IDLE,CYCLE>      (install only)
#   step=shared status=yes|no fstype=<fs>
#   step=distribute node=<node> status=ok|failed [reason=<unreachable|copy|
#                 time_cap|verify>]                      (not shared: per node)
#   step=distribute_summary ok=<count> failed=<comma list or none> [skipped=shared]
#   step=server_reach role=controller|node host=<name> status=ok|failed|unknown
#                 [reason=<dns|connect|timeout|tls|http|no_curl|other|not_asked>]
#                 (one per host; unknown = could not be asked, not a failure)
#   step=server_reach_summary url=<server url> ok=<count> failed=<comma list or
#                 none> unknown=<comma list or none>            (failed => partial)
#   step=slurm_conf status=ok|failed path=<file> backup=<file or none>
#                   chained=<program or none> interval=<n>        (install)
#   step=configless value=yes|no
#   step=reconfigure status=ok|failed
#   step=verify status=ok|partial [reason=program_not_applied|
#                 check_still_applied program=<what slurmctld runs>]
#                 (slurmctld, asked again after the reconfigure, runs our
#                 health check, or no longer does after --remove)
#   step=remove status=ok|noop [path=<file> backup=<file or none>]  (--remove)
#   step=uninstall_agents status=ok|failed interval=<n>   (--uninstall-agents)
#   step=done status=ok|partial|failed
# "partial" means setup finished but something is left to do by hand: nodes
# without the script, a slurm.conf still to be distributed, an unreloaded
# slurmctld. Exit status: 0 for ok and partial, 1 for failed.

set -u

# @@PARAMS@@

DEFAULT_INTERVAL=300
# Copying to the nodes: at most this many at once, and no new node is started
# once the budget (seconds) is spent; the rest is listed for copying by hand.
DIST_JOBS=${GWKS_DIST_JOBS:-16}
DIST_BUDGET=${GWKS_DIST_BUDGET:-180}
DIST_NODE_TIMEOUT=${GWKS_DIST_NODE_TIMEOUT:-60}
# Asking whether the server's address can be reached: seconds per host.
REACH_TIMEOUT=${GWKS_REACH_TIMEOUT:-10}
BEGIN_MARK='# BEGIN gpuworks'
END_MARK='# END gpuworks'
EOL_MARK='# gpuworks: the file had no final newline before this block.'
ORIG_PREFIX='#gpuworks-original# '
PARTIAL=0
WORK=''

say() {
	printf '%s\n' "$*"
}

mark() {
	printf 'GWKS_SLURM %s\n' "$*"
}

# clean makes a value safe for a key=value marker.
clean() {
	printf '%s' "$1" | tr -c 'A-Za-z0-9_.,/+:@-' '_'
}

have() {
	command -v "$1" >/dev/null 2>&1
}

fail() {
	say "ERROR: $*"
	mark "step=done status=failed"
	exit 1
}

finish() {
	if [ "$PARTIAL" = 1 ]; then
		say "Finished, but some steps above need your attention."
		mark "step=done status=partial"
	else
		say "Done."
		mark "step=done status=ok"
	fi
	exit 0
}

usage() {
	say "usage: sh -s -- [--interval SECONDS] [--no-node-reach] [--check] | [--remove | --uninstall-agents]"
}

# conf_value reads one `Key = value` line of `scontrol show config`.
conf_value() {
	printf '%s\n' "$CONFIG" | sed -n "s/^$1[[:space:]]*=[[:space:]]*//p" | head -n 1 | sed 's/[[:space:]]*$//'
}

# fact reads one key of the conf_facts output.
fact() {
	printf '%s\n' "$FACTS" | sed -n "s/^$1=//p" | head -n 1
}

# pre_fail reports a precheck that refused to go on.
pre_fail() {
	say "ERROR: $2"
	mark "step=precheck status=failed reason=$1"
	mark "step=done status=failed"
	exit 1
}

precheck() {
	if [ "$(id -u 2>/dev/null)" != 0 ]; then
		pre_fail not_root "this script changes slurm.conf and must run as root (use sudo)."
	fi
	if ! have scontrol; then
		pre_fail scontrol_not_found "scontrol not found: run this on the Slurm controller."
	fi
	if ! CONFIG=$(scontrol show config 2>/dev/null) || [ -z "$CONFIG" ]; then
		pre_fail slurmctld_unreachable "scontrol show config failed: is slurmctld running and reachable from here?"
	fi
	CLUSTER=$(conf_value ClusterName)
	if [ -z "$CLUSTER" ]; then
		pre_fail no_cluster_name "scontrol show config has no ClusterName."
	fi
	CONF=$(conf_value SLURM_CONF)
	[ -n "$CONF" ] || CONF=/etc/slurm/slurm.conf
	if [ ! -f "$CONF" ] || [ ! -r "$CONF" ]; then
		pre_fail conf_unreadable "cannot read slurm.conf at $CONF."
	fi
	# slurmd on a configless node keeps a copy of the cluster's configuration
	# for itself; editing that edits nothing the cluster reads.
	case "$CONF" in
	*/run/slurm/* | */conf-cache/*)
		pre_fail not_controller_conf "$CONF is the copy a configless slurmd keeps, not the controller's slurm.conf: run this on the machine that runs slurmctld."
		;;
	esac
	if ! on_slurmctld_host; then
		pre_fail not_slurmctld_host "this machine ($(hostname 2>/dev/null)) does not run slurmctld and is not one of the controller hosts ($(conf_value 'SlurmctldHost\[0\]')): run this on the machine that runs slurmctld."
	fi
	if [ "$MODE" = setup ]; then
		check_script_dir
	fi
	WORK=$(mktemp -d "${TMPDIR:-/tmp}/gwks-setup.XXXXXX") || fail "cannot create a temporary directory"
	trap 'rm -rf "$WORK"' EXIT
	say "Slurm cluster '$CLUSTER', slurm.conf at $CONF."
	mark "step=precheck status=ok cluster=$(clean "$CLUSTER")"
}

# check_script_dir refuses a script directory an account other than root
# could write: slurmd runs check.sh from it as root on every node, so whoever
# can change it, or a directory above it, runs code as root on all of them.
# Every directory from the script directory up to / must be root's (a share
# exported with root squash shows root's files as nobody's, and root cannot
# read the token there either) and writable by neither group nor others;
# above the script directory a sticky one (/tmp) is fine, since nobody else
# can replace what root put in it. A script directory that does not exist yet
# is made 0755 by root. One that already holds gpuworks's files and is only
# too open is tightened (reported in check mode); any other is refused, as is
# a directory whose owner or mode cannot be read.
check_script_dir() {
	allowed=${GWKS_OWNER_UID:-0}
	dir=$SCRIPT_DIR
	ours=no
	if [ -f "$dir/check.sh" ] || [ -f "$dir/token" ] || [ -f "$dir/chained" ]; then
		ours=yes
	fi
	first=yes
	result="status=ok"
	while :; do
		if [ -e "$dir" ]; then
			info=$(stat -L -c '%u %a' "$dir" 2>/dev/null) || info=''
			uid=${info%% *}
			mode=${info#* }
			if [ -z "$info" ] || [ -z "$mode" ]; then
				script_dir_unsafe "$dir" "?" "?" "its owner and mode cannot be read"
			fi
			if [ "$uid" != 0 ] && [ "$uid" != "$allowed" ]; then
				script_dir_unsafe "$dir" "$uid" "$mode" "it is owned by uid $uid, not by root (a share exported with root squash shows root's files as nobody's)"
			fi
			bits=$(printf '%s' "$mode" | awk '{ m = $1; while (length(m) < 4) m = "0" m
				s = substr(m, 1, 1) + 0; g = substr(m, 3, 1) + 0; o = substr(m, 4, 1) + 0
				w = (g == 2 || g == 3 || g == 6 || g == 7 || o == 2 || o == 3 || o == 6 || o == 7)
				t = (s == 1 || s == 3 || s == 5 || s == 7)
				printf "%s%s", (w ? "w" : "-"), (t ? "t" : "-") }')
			case "$bits" in
			w*)
				if [ "$first" = no ] && [ "$bits" = wt ]; then
					: # a sticky directory above ours
				elif [ "$first" = yes ] && [ "$ours" = yes ]; then
					if [ "$CHECK_ONLY" = yes ]; then
						say "The script directory $dir is writable by group or others ($mode); applying makes it not."
						result="status=plan action=tighten mode=$(clean "$mode")"
					elif chmod go-w "$dir"; then
						say "The script directory $dir was writable by group or others ($mode): made it not."
						result="status=ok action=tightened mode=$(clean "$mode")"
					else
						script_dir_unsafe "$dir" "$uid" "$mode" "it is writable by group or others and could not be changed"
					fi
				else
					script_dir_unsafe "$dir" "$uid" "$mode" "it is writable by group or others"
				fi
				;;
			esac
		fi
		[ "$dir" = / ] && break
		parent=$(dirname "$dir")
		[ "$parent" = "$dir" ] && break
		dir=$parent
		first=no
	done
	mark "step=script_dir $result path=$(clean "$SCRIPT_DIR")"
}

# script_dir_unsafe refuses the setup over a directory on the script
# directory's path: $1 the directory, $2 its owner, $3 its mode, $4 why.
script_dir_unsafe() {
	say "slurmd runs $SCRIPT_DIR/check.sh as root on every node, so only root may be able to change it or any directory above it."
	say "Make $1 owned by root and not writable by group or others (chown root $1; chmod go-w $1), or choose another script directory."
	mark "step=script_dir status=failed path=$(clean "$1") owner=$(clean "$2") mode=$(clean "$3")"
	pre_fail script_dir_unsafe "$1 is not safe for the health check: $4."
}

# on_slurmctld_host says whether this machine is a Slurm controller: a
# slurmctld process runs here, or its name (or an address) is one the
# configuration lists as SlurmctldHost (or the older ControlMachine,
# BackupController). `scontrol show config` answers from any machine that can
# reach the cluster, so asking it proves nothing about where the file is.
on_slurmctld_host() {
	for d in "${GWKS_PROC_DIR:-/proc}"/[0-9]*; do
		[ -r "$d/comm" ] || continue
		[ "$(cat "$d/comm" 2>/dev/null)" = slurmctld ] && return 0
	done
	short=$(hostname -s 2>/dev/null)
	full=$(hostname 2>/dev/null)
	addrs=$(hostname -I 2>/dev/null)
	for h in $(printf '%s\n' "$CONFIG" | sed -n -e 's/^SlurmctldHost[^=]*=[[:space:]]*//p' -e 's/^ControlMachine[[:space:]]*=[[:space:]]*//p' -e 's/^BackupController[[:space:]]*=[[:space:]]*//p'); do
		name=${h%%(*}
		addr=${h#*(}
		addr=${addr%)}
		[ "$h" = "$addr" ] && addr=''
		[ -n "$name" ] || continue
		if [ "$name" = "$short" ] || [ "$name" = "$full" ] || [ "${name%%.*}" = "$short" ]; then
			return 0
		fi
		if [ -n "$addr" ]; then
			for a in $addrs; do
				[ "$a" = "$addr" ] && return 0
			done
		fi
	done
	return 1
}

# install_check_script writes the health check next to its parameters: an
# atomic replace, because slurmd may be running the old one right now.
install_check_script() {
	# A directory made here is root's and 0755 (check_script_dir).
	if ! (umask 022 && mkdir -p "$SCRIPT_DIR"); then
		mark "step=check_script status=failed path=$(clean "$SCRIPT_DIR/check.sh")"
		fail "cannot create $SCRIPT_DIR"
	fi
	tmp="$SCRIPT_DIR/.check.sh.$$"
	cat >"$tmp.src" <<'GWKS_CHECK_SCRIPT_EOF'
@@CHECK_SCRIPT@@
GWKS_CHECK_SCRIPT_EOF
	# The check spreads its work over half of the health check interval.
	sed "s/^INTERVAL=[0-9]*\$/INTERVAL=$INTERVAL/" "$tmp.src" >"$tmp"
	rm -f "$tmp.src"
	chmod 0755 "$tmp" 2>/dev/null
	chown root:root "$tmp" 2>/dev/null || true
	if ! mv -f "$tmp" "$SCRIPT_DIR/check.sh"; then
		rm -f "$tmp"
		mark "step=check_script status=failed path=$(clean "$SCRIPT_DIR/check.sh")"
		fail "cannot write $SCRIPT_DIR/check.sh"
	fi
	say "Wrote the health check program $SCRIPT_DIR/check.sh."
	mark "step=check_script status=ok path=$(clean "$SCRIPT_DIR/check.sh")"
}

# install_token_file writes the enrollment token next to the check script,
# readable by root only (the check script is world readable and has none), and
# reads it back: the check on every node reads this file, so a controller that
# cannot read its own copy would leave every node unable to enroll.
install_token_file() {
	tmp="$SCRIPT_DIR/.token.$$"
	if (umask 077 && printf '%s\n' "$TOKEN" >"$tmp") && chmod 0600 "$tmp" &&
		{ chown root:root "$tmp" 2>/dev/null || true; } &&
		mv -f "$tmp" "$SCRIPT_DIR/token" &&
		[ "$(head -n 1 "$SCRIPT_DIR/token" 2>/dev/null)" = "$TOKEN" ]; then
		say "Wrote the enrollment token $SCRIPT_DIR/token (root only)."
		mark "step=token_file status=ok path=$(clean "$SCRIPT_DIR/token")"
		return 0
	fi
	rm -f "$tmp"
	mark "step=token_file status=failed path=$(clean "$SCRIPT_DIR/token")"
	fail "cannot write $SCRIPT_DIR/token or read it back"
}

# @@CONF_FUNCTIONS@@

# @@NODE_FUNCTIONS@@

# @@REACH_FUNCTIONS@@

# @@CHECK_FUNCTIONS@@

detect_configless() {
	case "$(conf_value SlurmctldParameters)" in
	*enable_configless*)
		say "slurmctld serves its configuration to the nodes (configless): they pick up the change on reconfigure."
		mark "step=configless value=yes"
		;;
	*)
		PARTIAL=1
		say "NOTICE: this cluster is not configless: copy $CONF to the compute nodes the way you usually"
		say "do, or the health check does not take effect there. scontrol reconfigure only tells slurmd to"
		say "re-read the slurm.conf on its own disk."
		mark "step=configless value=no"
		;;
	esac
}

reconfigure() {
	if scontrol reconfigure </dev/null; then
		say "scontrol reconfigure: ok."
		mark "step=reconfigure status=ok"
	else
		say "ERROR: scontrol reconfigure failed: run it by hand once slurmctld is reachable."
		mark "step=reconfigure status=failed"
		mark "step=done status=failed"
		exit 1
	fi
}

# verify_applied asks slurmctld again, after the reconfigure, whether it runs
# our health check (present) or no longer does (absent). A setting in an
# included file, or a controller that has not finished reading, shows here.
# Nothing is wrong with the nodes then, but the health check does not do what
# it was set up to do: the run ends partial, with what slurmctld said.
verify_applied() {
	i=0
	while :; do
		CONFIG=$(scontrol show config 2>/dev/null) || CONFIG=''
		running=$(conf_value HealthCheckProgram)
		if [ "$1" = present ] && [ "$running" = "$SCRIPT_DIR/check.sh" ]; then
			say "slurmctld runs the gpuworks health check."
			mark "step=verify status=ok"
			return 0
		fi
		if [ "$1" = absent ] && { [ "$running" != "$SCRIPT_DIR/check.sh" ] || [ "${KEEPS_OURS:-no}" = yes ]; }; then
			mark "step=verify status=ok"
			return 0
		fi
		i=$((i + 1))
		[ "$i" -lt 3 ] || break
		sleep 1
	done
	PARTIAL=1
	if [ "$1" = present ]; then
		say "WARNING: after the reconfigure slurmctld still runs HealthCheckProgram=${running:-(none)}, not $SCRIPT_DIR/check.sh."
		say "Look for HealthCheckProgram in a file that $CONF includes, and for slurmctld not having finished its reconfigure."
		mark "step=verify status=partial reason=program_not_applied program=$(clean "${running:-none}")"
	else
		say "WARNING: after the reconfigure slurmctld still runs $SCRIPT_DIR/check.sh."
		mark "step=verify status=partial reason=check_still_applied program=$(clean "$running")"
	fi
}

do_setup() {
	precheck
	analyze_conf
	choose_interval
	install_check_script
	install_token_file
	plan_chain
	# A re-run re-arms install mode.
	rm -f "$SCRIPT_DIR/uninstall-agents"
	detect_shared
	if [ "$SHARED" = no ]; then
		sync_nodes
	else
		mark "step=distribute_summary ok=0 failed=none skipped=shared"
	fi
	check_server_reach
	update_conf
	detect_configless
	reconfigure
	verify_applied present
	say "The agent is installed on GPU nodes by the health check, within one HealthCheckInterval (${INTERVAL} s)."
	finish
}

do_remove() {
	precheck
	analyze_conf
	if [ "$(fact has_block)" != yes ]; then
		say "Nothing to remove: $CONF has no gpuworks block."
		mark "step=remove status=noop"
		mark "step=done status=ok"
		exit 0
	fi
	conf_rewrite uncomment >"$WORK/new" || fail "cannot read $CONF"
	# A cluster that named our program by hand keeps it after the removal.
	KEEPS_OURS=no
	if grep -Eiq "^[[:space:]]*HealthCheckProgram[[:space:]]*=[[:space:]]*$SCRIPT_DIR/check.sh([[:space:]#]|\$)" "$WORK/new"; then
		KEEPS_OURS=yes
	fi
	store_conf "$WORK/new"
	rm -f "$SCRIPT_DIR/uninstall-agents" "$SCRIPT_DIR/token"
	say "Removed the gpuworks block from $CONF and restored the original HealthCheck settings (backup: $BACKUP)."
	mark "step=remove status=ok path=$(clean "$CONF") backup=$(clean "$BACKUP")"
	reconfigure
	verify_applied absent
	detect_configless
	say "Agents already installed on the nodes keep running. To remove them too, run the"
	say "--uninstall-agents command, wait one HealthCheckInterval, then run --remove."
	finish
}

do_uninstall_agents() {
	precheck
	analyze_conf
	if [ "$(fact has_block)" != yes ] || [ ! -x "$SCRIPT_DIR/check.sh" ]; then
		say "ERROR: the health check is not installed: run the setup command first."
		mark "step=uninstall_agents status=failed interval=0"
		fail "health check not installed"
	fi
	interval=$(fact block_interval)
	case "$interval" in '' | 0 | *[!0-9]*) interval=$DEFAULT_INTERVAL ;; esac
	if [ "$(conf_value HealthCheckProgram)" != "$SCRIPT_DIR/check.sh" ]; then
		PARTIAL=1
		say "NOTICE: slurmctld is not running with the gpuworks health check yet: run scontrol reconfigure."
	fi
	if ! date >"$SCRIPT_DIR/uninstall-agents"; then
		fail "cannot write $SCRIPT_DIR/uninstall-agents"
	fi
	detect_shared
	if [ "$SHARED" = no ]; then
		sync_nodes
	else
		mark "step=distribute_summary ok=0 failed=none skipped=shared"
	fi
	say "Uninstall mode is armed. Wait one HealthCheckInterval (${interval} s), then run the --remove command."
	mark "step=uninstall_agents status=ok interval=$interval"
	finish
}

main() {
	MODE=setup
	INTERVAL_OPT=''
	CHECK_ONLY=no
	REACH_NODES=yes
	while [ $# -gt 0 ]; do
		case "$1" in
		--check) CHECK_ONLY=yes ;;
		--no-node-reach) REACH_NODES=no ;;
		--remove | --uninstall-agents)
			if [ "$MODE" != setup ] && [ "$MODE" != "${1#--}" ]; then
				usage
				mark "step=precheck status=failed reason=bad_arguments"
				exit 1
			fi
			MODE=${1#--}
			;;
		--interval)
			[ $# -ge 2 ] || {
				usage
				mark "step=precheck status=failed reason=bad_arguments"
				exit 1
			}
			INTERVAL_OPT=$2
			shift
			;;
		--interval=*) INTERVAL_OPT=${1#--interval=} ;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			say "unknown argument: $1"
			usage
			mark "step=precheck status=failed reason=bad_arguments"
			exit 1
			;;
		esac
		shift
	done
	if [ -n "$INTERVAL_OPT" ]; then
		case "$INTERVAL_OPT" in *[!0-9]*)
			say "--interval wants a positive number of seconds"
			mark "step=precheck status=failed reason=bad_arguments"
			exit 1
			;;
		esac
		if [ "$INTERVAL_OPT" -le 0 ]; then
			say "--interval wants a positive number of seconds"
			mark "step=precheck status=failed reason=bad_arguments"
			exit 1
		fi
	fi
	if [ "$CHECK_ONLY" = yes ] && [ "$MODE" != setup ]; then
		usage
		mark "step=precheck status=failed reason=bad_arguments"
		exit 1
	fi
	[ "$CHECK_ONLY" = yes ] && do_check
	case "$MODE" in
	remove) do_remove ;;
	uninstall-agents) do_uninstall_agents ;;
	*) do_setup ;;
	esac
}

# Everything above is only definitions, so a script piped into `sh` has been
# read completely before anything runs; stdin goes to /dev/null so no command
# (ssh, say) can swallow what is left of it.
main "$@" </dev/null
exit $?
