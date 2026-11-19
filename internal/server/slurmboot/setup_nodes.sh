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
	# (gwks_dir_unsafe, sent along), before anything is copied there: a node
	# where another account could change it gets nothing, the token
	# included. Copying writes into the files there, so on a node the
	# directory being itself too open is unsafe too. A missing directory is
	# made root's with umask 022 and checked again once made; an existing one
	# keeps its mode. The token file exists with mode 0600 before anything is
	# copied into it: copying overwrites the contents and keeps that mode.
	nodecheck="$DIR_CHECK
r=\$(gwks_dir_unsafe '$SCRIPT_DIR'); if [ -z \"\$r\" ]; then echo GWKS_SAFE; else echo \"GWKS_UNSAFE \$r\"; fi"
	# prep checks again itself (ssh has no separate pass, and the directory
	# may be made by it): an unsafe one prints GWKS_UNSAFE and stops.
	prep="$DIR_CHECK
gwks_safe() { r=\$(gwks_dir_unsafe '$SCRIPT_DIR'); [ -z \"\$r\" ] && return 0; echo \"GWKS_UNSAFE \$r\"; exit 1; }
gwks_safe && (umask 022 && mkdir -p '$SCRIPT_DIR') && gwks_safe && (umask 077 && : > '$SCRIPT_DIR/token') && chmod 0600 '$SCRIPT_DIR/token'"
	for f in $ABSENT; do
		prep="$prep && rm -f '$SCRIPT_DIR/$f'"
	done
	fin="chmod 0755 '$SCRIPT_DIR/check.sh' && chmod 0600 '$SCRIPT_DIR/token'"
	probe="$DIR_CHECK
[ -z \"\$(gwks_dir_unsafe '$SCRIPT_DIR')\" ] && test -x '$SCRIPT_DIR/check.sh' && test -s '$SCRIPT_DIR/token' && echo GWKS_OK"
	mkdir -p "$WORK/dist"
	if have clush || { have pdsh && have pdcp; }; then
		# One pass over every node first: only safe nodes are copied to.
		tool=pdsh
		have clush && tool=clush
		csv=$(printf '%s\n' $NODES | tr '\n' ',' | sed 's/,$//')
		checked=$(budgeted "$tool" -w "$csv" "$nodecheck" </dev/null 2>/dev/null)
		safe=''
		for n in $NODES; do
			if printf '%s\n' "$checked" | grep -Fxq "$n: GWKS_SAFE"; then
				safe="$safe $n"
			elif line=$(printf '%s\n' "$checked" | grep -F "$n: GWKS_UNSAFE " | head -n 1) && [ -n "$line" ]; then
				echo failed unsafe >"$WORK/dist/$(clean "$n")"
				printf '%s\n' "${line#*GWKS_UNSAFE }" >"$WORK/dist/$(clean "$n").why"
			else
				echo failed unreachable >"$WORK/dist/$(clean "$n")"
			fi
		done
		csv=$(printf '%s\n' $safe | tr '\n' ',' | sed 's/,$//')
	fi
	if [ -n "${csv:-}" ] && have clush; then
		say "Copying with clush to: $csv"
		budgeted clush -w "$csv" "$prep" </dev/null >/dev/null 2>&1
		for f in $FILES; do
			budgeted clush -w "$csv" --copy "$SCRIPT_DIR/$f" --dest "$SCRIPT_DIR/" </dev/null >/dev/null 2>&1
		done
		budgeted clush -w "$csv" "$fin" </dev/null >/dev/null 2>&1
		verified=$(budgeted clush -w "$csv" "$probe" </dev/null 2>/dev/null)
		for n in $safe; do
			if printf '%s\n' "$verified" | grep -Fxq "$n: GWKS_OK"; then echo ok >"$WORK/dist/$(clean "$n")"; else echo failed unreachable >"$WORK/dist/$(clean "$n")"; fi
		done
	elif [ -n "${csv:-}" ] && have pdsh && have pdcp; then
		say "Copying with pdsh/pdcp to: $csv"
		budgeted pdsh -w "$csv" "$prep" </dev/null >/dev/null 2>&1
		for f in $FILES; do
			budgeted pdcp -w "$csv" "$SCRIPT_DIR/$f" "$SCRIPT_DIR/" </dev/null >/dev/null 2>&1
		done
		budgeted pdsh -w "$csv" "$fin" </dev/null >/dev/null 2>&1
		verified=$(budgeted pdsh -w "$csv" "$probe" </dev/null 2>/dev/null)
		for n in $safe; do
			if printf '%s\n' "$verified" | grep -Fxq "$n: GWKS_OK"; then echo ok >"$WORK/dist/$(clean "$n")"; else echo failed unreachable >"$WORK/dist/$(clean "$n")"; fi
		done
	elif ! have clush && ! { have pdsh && have pdcp; }; then
		csv=$(printf '%s\n' $NODES | tr '\n' ',' | sed 's/,$//')
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
	failed=$(printf '%s\n' $failed | tr '\n' ',' | sed 's/^,//; s/,$//')
	mark "step=distribute_summary ok=$nok failed=${failed:-none}"
	if [ -n "$failed" ]; then
		PARTIAL=1
		say "These nodes did not get the scripts (no passwordless ssh as root, or not done within ${DIST_BUDGET} s): $failed"
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
