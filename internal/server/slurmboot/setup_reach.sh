# Whether the server's address can be reached from the machines that need it.
# (A fragment of the setup script: setup.sh pulls it in where it is needed.)
#
# The health check on every GPU node asks the server (token-status, enroll)
# at SERVER_URL, so an address the nodes cannot reach leaves a cluster that
# applied cleanly and never gets a node. Both the install and --check ask
# $SERVER_URL/healthz from this controller and, when they can reach the nodes
# (clush, pdsh or passwordless root ssh, the way the scripts are copied), from
# each GPU node of the cluster. Nothing is written anywhere: the question is a
# read-only curl. A host that cannot reach the server makes the run partial.

# reach_reason words a curl exit status: dns, connect (refused or no route),
# timeout, tls, http (the server answered with an error), no_curl or other.
reach_reason() {
	case "$1" in
	0) printf '%s\n' ok ;;
	127) printf '%s\n' no_curl ;;
	6) printf '%s\n' dns ;;
	7) printf '%s\n' connect ;;
	28) printf '%s\n' timeout ;;
	22) printf '%s\n' http ;;
	35 | 51 | 53 | 54 | 58 | 59 | 60 | 66 | 77 | 80 | 82 | 83 | 90 | 91) printf '%s\n' tls ;;
	*) printf '%s\n' other ;;
	esac
}

# reach_command is what each host runs: it prints GWKS_REACH_RC=<curl's exit
# status> (127 when there is no curl) and nothing else that matters.
reach_command() {
	printf '%s' "curl -fsS --max-time $REACH_TIMEOUT -o /dev/null '$SERVER_URL/healthz' >/dev/null 2>&1; echo GWKS_REACH_RC=\$?"
}

# gpu_nodes puts into GPU_NODES the nodes (but this machine) that Slurm gives
# a gpu Gres, as `scontrol -o show node` lists them: those are the nodes the
# health check puts an agent on.
gpu_nodes() {
	self_short=$(hostname -s 2>/dev/null)
	self_full=$(hostname 2>/dev/null)
	GPU_NODES=''
	for n in $(scontrol -o show node 2>/dev/null </dev/null | awk '
		{
			name = ""; gres = ""
			for (i = 1; i <= NF; i++) {
				f = $i
				if (f ~ /^Reason=/) break
				if (f ~ /^NodeName=/ && name == "") name = substr(f, 10)
				else if (f ~ /^Gres=/ && gres == "") gres = substr(f, 6)
			}
			if (name !~ /^[A-Za-z0-9._-]+$/) next
			k = split(gres, items, ",")
			for (j = 1; j <= k; j++) if (items[j] ~ /^gpu(:|\(|$)/) { print name; break }
		}' | sort -u); do
		[ "$n" = "$self_short" ] || [ "$n" = "$self_full" ] || GPU_NODES="$GPU_NODES $n"
	done
}

# reach_mark reports one host: role (controller|node), its name, and curl's
# exit status, or "" when the host could not be asked.
reach_mark() {
	role=$1
	host=$2
	rc=$3
	if [ -z "$rc" ]; then
		say "  $host: not asked (no way to run a command there)"
		mark "step=server_reach role=$role host=$(clean "$host") status=unknown reason=not_asked"
		REACH_UNKNOWN="$REACH_UNKNOWN $host"
		return 0
	fi
	reason=$(reach_reason "$rc")
	if [ "$reason" = ok ]; then
		say "  $host: reaches $SERVER_URL"
		mark "step=server_reach role=$role host=$(clean "$host") status=ok"
		REACH_OK=$((REACH_OK + 1))
	else
		say "  $host: CANNOT reach $SERVER_URL ($reason)"
		mark "step=server_reach role=$role host=$(clean "$host") status=failed reason=$reason"
		REACH_FAILED="$REACH_FAILED $host"
	fi
}

# reach_node_ssh asks one node with ssh and leaves curl's exit status (or
# nothing, when ssh could not run the command) in $WORK/reach/<node>. It runs
# in the background.
reach_node_ssh() {
	n=$1
	out="$WORK/reach/$(clean "$n")"
	# shellcheck disable=SC2086
	got=$(node_bounded ssh $REACH_SSH_OPTS "$n" "$REACH_CMD" </dev/null 2>/dev/null | sed -n 's/^GWKS_REACH_RC=//p' | head -n 1)
	printf '%s\n' "$got" >"$out"
}

# reach_nodes asks every GPU node, with the tool the copy to the nodes uses
# (clush, pdsh, else ssh, REACH_JOBS at a time and within the same time
# budget), and reports each.
reach_nodes() {
	gpu_nodes
	[ -n "$GPU_NODES" ] || return 0
	REACH_CMD=$(reach_command)
	csv=$(nodes_csv "$GPU_NODES")
	mkdir -p "$WORK/reach"
	if have clush; then
		got=$(budgeted clush -w "$csv" "$REACH_CMD" </dev/null 2>/dev/null)
		for n in $GPU_NODES; do
			printf '%s\n' "$got" | awk -v k="$n:" '$1 == k && $2 ~ /^GWKS_REACH_RC=/ { print substr($2, 15); exit }' >"$WORK/reach/$(clean "$n")"
		done
	elif have pdsh; then
		got=$(budgeted pdsh -w "$csv" "$REACH_CMD" </dev/null 2>/dev/null)
		for n in $GPU_NODES; do
			printf '%s\n' "$got" | awk -v k="$n:" '$1 == k && $2 ~ /^GWKS_REACH_RC=/ { print substr($2, 15); exit }' >"$WORK/reach/$(clean "$n")"
		done
	else
		REACH_SSH_OPTS="-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new"
		started=$(date +%s)
		running=0
		for n in $GPU_NODES; do
			if [ $(($(date +%s) - started)) -ge "$DIST_BUDGET" ]; then
				: >"$WORK/reach/$(clean "$n")"
				continue
			fi
			reach_node_ssh "$n" &
			running=$((running + 1))
			if [ "$running" -ge "$DIST_JOBS" ]; then
				wait
				running=0
			fi
		done
		wait
	fi
	for n in $GPU_NODES; do
		reach_mark node "$n" "$(cat "$WORK/reach/$(clean "$n")" 2>/dev/null)"
	done
}

# check_server_reach is the whole question: the controller, then (unless
# --no-node-reach) its GPU nodes. A host that cannot reach the server makes
# the run partial; one that could not be asked is only said.
check_server_reach() {
	REACH_OK=0
	REACH_FAILED=''
	REACH_UNKNOWN=''
	say "Checking that $SERVER_URL can be reached from the cluster."
	if have curl; then
		curl -fsS --max-time "$REACH_TIMEOUT" -o /dev/null "$SERVER_URL/healthz" >/dev/null 2>&1
		rc=$?
	else
		rc=127
	fi
	reach_mark controller "$(hostname -s 2>/dev/null || echo controller)" "$rc"
	if [ "$REACH_NODES" = yes ]; then
		reach_nodes
	fi
	failed=$(nodes_csv "$REACH_FAILED")
	unknown=$(nodes_csv "$REACH_UNKNOWN")
	mark "step=server_reach_summary url=$(clean "$SERVER_URL") ok=$REACH_OK failed=${failed:-none} unknown=${unknown:-none}"
	if [ -n "$failed" ]; then
		PARTIAL=1
		say "WARNING: these hosts cannot reach $SERVER_URL: $failed"
		say "The agents cannot enroll from them. Use an address of this server that every GPU node can reach (not a temporary public address), then run this again."
	fi
}
