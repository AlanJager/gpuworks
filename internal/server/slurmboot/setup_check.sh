# The check-only pass: what the install would do, said without doing it.
# (A fragment of the setup script: setup.sh pulls it in where it is needed.)

# existing_ancestor prints the nearest directory of a path that exists, which
# is where the filesystem under a not yet created script directory is read.
existing_ancestor() {
	d=$1
	while [ ! -d "$d" ] && [ "$d" != / ]; do
		d=$(dirname "$d")
	done
	printf '%s\n' "$d"
}

# plan_comments lists the HealthCheck settings the install would turn into
# "#gpuworks-original#" comments, as "line key value" (our own block is not
# touched, and a commented setting is not one).
plan_comments() {
	awk -v begin="$BEGIN_MARK" -v end="$END_MARK" '
$0 == begin { inblk = 1; next }
inblk { if ($0 == end) inblk = 0; next }
{
	l = tolower($0)
	if (l ~ /^[ \t]*healthcheck(program|interval|nodestate)[ \t]*=/) {
		key = l
		sub(/^[ \t]*/, "", key)
		sub(/[ \t]*=.*/, "", key)
		v = $0
		sub(/^[^=]*=/, "", v)
		sub(/#.*/, "", v)
		sub(/^[ \t]+/, "", v)
		sub(/[ \t].*/, "", v)
		print NR, key, (v == "" ? "none" : v)
	}
}' "$CONF"
}

# do_check reports the plan of the install and changes nothing: no file is
# written, nothing is copied, slurm.conf is not touched and slurmctld is not
# told to reconfigure. The only thing it creates is the temporary directory
# precheck makes, which is removed when it ends. Every fact is worked out by
# the same functions the install uses, and each marker carries the same
# keys the install's marker of that step has.
do_check() {
	precheck
	analyze_conf
	choose_interval
	say "CHECK ONLY: nothing is changed on this cluster."
	check="$SCRIPT_DIR/check.sh"
	action='write'
	[ -e "$check" ] && action=replace
	say "Would write the health check program $check ($action)."
	mark "step=check_script status=plan path=$(clean "$check") action=$action"
	say "Would write the enrollment token $SCRIPT_DIR/token (root only)."
	mark "step=token_file status=plan path=$(clean "$SCRIPT_DIR/token")"
	settle_chain
	if [ -n "$CHAINED" ]; then
		say "The existing health check program $CHAINED would run after ours and keep its exit status, on nodes in the states it asked for ($ORIG_STATE)."
	fi
	mark "step=chained status=plan program=$(clean "${CHAINED:-none}") node_state=$(chained_state)"
	rearm=no
	[ -e "$SCRIPT_DIR/uninstall-agents" ] && rearm=yes
	mark "step=install_mode status=plan rearm=$rearm"
	SHARED_PROBE=$(existing_ancestor "$SCRIPT_DIR")
	detect_shared
	files='check.sh,token'
	[ -z "$CHAINED" ] || files="$files,chained"
	if [ "$SHARED" = no ]; then
		list_nodes
		csv=$(nodes_csv "$NODES")
		say "Would copy $files to: ${csv:-no other node (sinfo lists none)}"
		mark "step=distribute_plan nodes=${csv:-none} files=$files"
	else
		mark "step=distribute_plan nodes=none files=$files skipped=shared"
	fi
	check_server_reach
	build_conf
	plan_comments | while read -r nr key val; do
		case "$key" in
		healthcheckprogram) key=HealthCheckProgram ;;
		healthcheckinterval) key=HealthCheckInterval ;;
		*) key=HealthCheckNodeState ;;
		esac
		say "Would comment $key (line $nr of $CONF, value $val): the cluster's own setting is kept as a comment."
		mark "step=conf_comment line=$nr key=$key value=$(clean "$val")"
	done
	changed=yes
	if cmp -s "$CONF" "$WORK/new"; then
		changed=no
		say "$CONF already has the gpuworks block as it would write it."
	else
		say "Would add to $CONF (a backup is made first):"
		sed -n "/^$BEGIN_MARK\$/,\$p" "$WORK/new" | sed 's/^/    /'
	fi
	mark "step=slurm_conf status=plan path=$(clean "$CONF") chained=$(clean "${CHAINED:-none}") interval=$INTERVAL node_state=$(clean "$BLOCK_STATE") changed=$changed"
	detect_configless
	mark "step=reconfigure status=plan"
	if [ "$PARTIAL" = 1 ]; then
		say "Checked; some points above need your attention."
		mark "step=done status=partial mode=check"
	else
		say "Checked; nothing stands in the way."
		mark "step=done status=ok mode=check"
	fi
	exit 0
}
