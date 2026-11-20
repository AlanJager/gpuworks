# Getting the scripts to the compute nodes when the script directory is not shared.
# (A fragment of the setup script: setup.sh pulls it in where it is needed.)

# fstype_of prints the filesystem type under a directory. findmnt tells
# "fuse.sshfs" and "nfs4"; stat and df are the fallbacks.
fstype_of() {
	t=''
	if have findmnt; then
		t=$(findmnt -n -o FSTYPE -T "$1" 2>/dev/null | head -n 1)
	fi
	case "$t" in '' | UNKNOWN*)
		t=''
		if have stat; then
			t=$(stat -f -c %T "$1" 2>/dev/null)
		fi
		;;
	esac
	case "$t" in '' | UNKNOWN*)
		t=$(df -PT "$1" 2>/dev/null | awk 'NR == 2 { print $2 }')
		;;
	esac
	case "$t" in '' | UNKNOWN*) t=unknown ;; esac
	printf '%s\n' "$t"
}

# detect_shared decides whether every node already sees SCRIPT_DIR: a network
# or cluster filesystem does, local storage does not. fuseblk is a local block
# device behind FUSE, not a shared filesystem.
detect_shared() {
	FSTYPE=$(fstype_of "${SHARED_PROBE:-$SCRIPT_DIR}")
	case "$FSTYPE" in
	nfs | nfs4 | lustre | gpfs | beegfs | ceph | cephfs | cifs | smb3 | smbfs | glusterfs | fuse | fuse.*)
		SHARED=yes
		say "$SCRIPT_DIR is on a shared filesystem ($FSTYPE): every node sees the scripts, nothing to copy."
		;;
	*)
		SHARED=no
		say "$SCRIPT_DIR is on local storage ($FSTYPE): the scripts are copied to the compute nodes."
		;;
	esac
	mark "step=shared status=$SHARED fstype=$(clean "$FSTYPE")"
}

# Node lists are built by appending a space and a name for each host.
nodes_csv() {
	printf '%s' "${1# }" | tr ' ' ','
}

# list_nodes puts into NODES every node the cluster lists but this machine.
list_nodes() {
	self_short=$(hostname -s 2>/dev/null)
	self_full=$(hostname 2>/dev/null)
	NODES=''
	for n in $(sinfo -h -a -N -o %N 2>/dev/null </dev/null | sort -u); do
		[ "$n" = "$self_short" ] || [ "$n" = "$self_full" ] || NODES="$NODES $n"
	done
}

# sync_nodes brings <script dir> on the compute nodes in line with the
# controller's: check.sh and token (mode 0600) always, chained and uninstall-agents when they exist
# here and removed there when they do not. Every node the cluster lists gets
# them, CPU-only ones too: the cluster's own health check, which runs through
# check.sh, must keep working there. The nodes are done in parallel (clush or
# pdsh when they exist; else ssh, DIST_JOBS at a time) and within a time
# budget: a node not started by then is listed, like a failed one, for copying
# by hand. A failed node never stops the rest.
sync_nodes() {
	FILES='check.sh token'
	ABSENT=''
	for f in chained uninstall-agents; do
		if [ -f "$SCRIPT_DIR/$f" ]; then
			FILES="$FILES $f"
		else
			ABSENT="$ABSENT $f"
		fi
	done
	list_nodes
	if [ -z "$NODES" ]; then
		say "sinfo lists no other nodes: nothing to copy."
		mark "step=distribute_summary ok=0 failed=none"
		return 0
	fi
	# Each node's script directory is checked as the controller's is
	# (gwks_dir_unsafe, sent along) by prep, before anything is copied there:
	# a node where another account could change it prints GWKS_UNSAFE and
	# gets nothing, the token included, and only a node that printed
	# GWKS_PREPPED is copied to. Copying writes into the files there, so on a
	# node the directory being itself too open is unsafe too. A missing
	# directory is made root's with umask 022 and checked again once made; an
	# existing one keeps its mode. The token file exists with mode 0600 before
	# anything is copied into it: copying overwrites the contents and keeps
	# that mode. The text needs a POSIX login shell for root on the nodes.
	prep="$DIR_CHECK
gwks_safe() { r=\$(gwks_dir_unsafe '$SCRIPT_DIR'); [ -z \"\$r\" ] && return 0; echo \"GWKS_UNSAFE \$r\"; exit 1; }
gwks_safe && (umask 022 && mkdir -p '$SCRIPT_DIR') && gwks_safe && (umask 077 && : > '$SCRIPT_DIR/token') && chmod 0600 '$SCRIPT_DIR/token'"
	for f in $ABSENT; do
		prep="$prep && rm -f '$SCRIPT_DIR/$f'"
	done
	prep="$prep && echo GWKS_PREPPED"
	fin="chmod 0755 '$SCRIPT_DIR/check.sh' && chmod 0600 '$SCRIPT_DIR/token'"
	probe="$DIR_CHECK
[ -z \"\$(gwks_dir_unsafe '$SCRIPT_DIR')\" ] && test -x '$SCRIPT_DIR/check.sh' && test -s '$SCRIPT_DIR/token' && echo GWKS_OK"
	mkdir -p "$WORK/dist"
	tool=''
	if have clush; then
		tool=clush
	elif have pdsh && have pdcp; then
		tool=pdsh
	fi
	if [ -n "$tool" ]; then
		csv=$(nodes_csv "$NODES")
		say "Copying with $tool to: $csv"
		said=$(fanout "$csv" "$prep")
		safe=''
		for n in $NODES; do
			if node_said "$said" "$n" GWKS_PREPPED >/dev/null; then
				safe="$safe $n"
			elif why=$(node_said "$said" "$n" GWKS_UNSAFE); then
				echo failed unsafe >"$WORK/dist/$(clean "$n")"
				printf '%s\n' "$why" >"$WORK/dist/$(clean "$n").why"
			else
				echo failed unreachable >"$WORK/dist/$(clean "$n")"
			fi
		done
		csv=$(nodes_csv "$safe")
		if [ -n "$csv" ]; then
			for f in $FILES; do
				if [ "$tool" = clush ]; then
					budgeted clush -w "$csv" --copy "$SCRIPT_DIR/$f" --dest "$SCRIPT_DIR/" </dev/null >/dev/null 2>&1
				else
					budgeted pdcp -w "$csv" "$SCRIPT_DIR/$f" "$SCRIPT_DIR/" </dev/null >/dev/null 2>&1
				fi
			done
			fanout "$csv" "$fin" >/dev/null
			verified=$(fanout "$csv" "$probe")
			for n in $safe; do
				if node_said "$verified" "$n" GWKS_OK >/dev/null; then echo ok >"$WORK/dist/$(clean "$n")"; else echo failed verify >"$WORK/dist/$(clean "$n")"; fi
			done
		fi
	else
		csv=$(nodes_csv "$NODES")
		ssh_opts="-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new"
		say "Copying with ssh to: $csv (at most $DIST_JOBS at a time)"
		started=$(date +%s)
		running=0
		for n in $NODES; do
			if [ $(($(date +%s) - started)) -ge "$DIST_BUDGET" ]; then
				echo failed time_cap >"$WORK/dist/$(clean "$n")"
				continue
			fi
			sync_node_ssh "$n" &
			running=$((running + 1))
			if [ "$running" -ge "$DIST_JOBS" ]; then
				wait
				running=0
			fi
		done
		wait
	fi
	nok=0
	failed=''
	for n in $NODES; do
		result=$(cat "$WORK/dist/$(clean "$n")" 2>/dev/null)
		case "$result" in
		ok)
			nok=$((nok + 1))
			say "  $n: OK"
			mark "step=distribute node=$(clean "$n") status=ok"
			;;
		*)
			reason=${result#failed }
			case "$reason" in unreachable | copy | time_cap | verify | unsafe) ;; *) reason=unreachable ;; esac
			failed="$failed $n"
			say "  $n: FAILED ($reason)"
			if [ "$reason" = unsafe ] && why=$(cat "$WORK/dist/$(clean "$n").why" 2>/dev/null); then
				rest=${why#*|}
				say "    ${rest%%|*} is not safe for the health check there: ${why##*|}. Make it owned by root and not writable by group or others."
			fi
			mark "step=distribute node=$(clean "$n") status=failed reason=$reason"
			;;
		esac
	done
	failed=$(nodes_csv "$failed")
	mark "step=distribute_summary ok=$nok failed=${failed:-none}"
	if [ -n "$failed" ]; then
		PARTIAL=1
		say "These nodes did not get the scripts (the reason is on each line above: no passwordless ssh as root, not done within ${DIST_BUDGET} s, or an unsafe script directory there): $failed"
		say "Copy these files to the same path on each of them, mode 0755 for check.sh and 0600 for token:"
		for f in $FILES; do
			say "  $SCRIPT_DIR/$f"
		done
		say "(mkdir -p $SCRIPT_DIR first; or mount shared storage there and run this again.)"
	fi
	# Not shared: what is copied now is all a node has. This is a thing to
	# know, not a failure: the run is not partial because of it.
	say "NOTICE: $SCRIPT_DIR is not on shared storage, so a node added to the cluster later has none of these files, and its health check"
	say "(which is this script) does not exist there. Run this command again after adding nodes, or put $SCRIPT_DIR on shared storage."
}

# budgeted runs a command for at most DIST_BUDGET seconds when timeout(1) exists.
budgeted() {
	if have timeout; then
		timeout "$DIST_BUDGET" "$@"
	else
		"$@"
	fi
}

# fanout runs a command on the nodes in $1 with clush or pdsh ($tool) and
# prints what they said, each line as "<node>: <line>". pdsh expands %h, %u
# and %n in the command, so its % signs are doubled.
fanout() {
	if [ "$tool" = clush ]; then
		budgeted clush -w "$1" "$2" </dev/null 2>/dev/null
	else
		budgeted pdsh -w "$1" "$(printf '%s' "$2" | sed 's/%/%%/g')" </dev/null 2>/dev/null
	fi
}

# node_said finds the line node $2 said starting with word $3 in output $1
# and prints the rest of it; it fails when the node said no such line. The
# node name is matched whole, so cn1 is not gcn1.
node_said() {
	printf '%s\n' "$1" | awk -v p="$2: $3" 'index($0, p) == 1 && (length($0) == length(p) || substr($0, length(p) + 1, 1) == " ") { print substr($0, length(p) + 2); found = 1; exit } END { exit !found }'
}

# sync_node_ssh does one node with ssh and scp and leaves "ok" or "failed
# <reason>" in $WORK/dist/<node>. It runs in the background.
sync_node_ssh() {
	n=$1
	out="$WORK/dist/$(clean "$n")"
	# shellcheck disable=SC2086
	if ! said=$(node_bounded ssh $ssh_opts "$n" "$prep" </dev/null 2>/dev/null); then
		case "$said" in
		*"GWKS_UNSAFE "*)
			echo failed unsafe >"$out"
			printf '%s\n' "$said" | sed -n 's/^GWKS_UNSAFE //p' | head -n 1 >"$out.why"
			;;
		*) echo failed unreachable >"$out" ;;
		esac
		return 0
	fi
	for f in $FILES; do
		# shellcheck disable=SC2086
		if ! node_bounded scp -q $ssh_opts "$SCRIPT_DIR/$f" "$n:$SCRIPT_DIR/" </dev/null >/dev/null 2>&1; then
			echo failed copy >"$out"
			return 0
		fi
	done
	# shellcheck disable=SC2086
	if ! node_bounded ssh $ssh_opts "$n" "$fin" </dev/null >/dev/null 2>&1; then
		echo failed copy >"$out"
		return 0
	fi
	# shellcheck disable=SC2086
	if node_bounded ssh $ssh_opts "$n" "$probe" </dev/null 2>/dev/null | grep -Fxq GWKS_OK; then
		echo ok >"$out"
	else
		echo failed verify >"$out"
	fi
}

# node_bounded keeps one node's command from holding a whole batch.
node_bounded() {
	if have timeout; then
		timeout "$DIST_NODE_TIMEOUT" "$@"
	else
		"$@"
	fi
}
