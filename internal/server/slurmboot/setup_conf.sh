# Editing slurm.conf: reading what is there, our block, and the chained program.
# (A fragment of the setup script: setup.sh pulls it in where it is needed.)

# conf_facts summarises slurm.conf for the decisions below. Lines inside our
# own block are looked at only for the interval it holds. Keys are case
# insensitive in slurm.conf, and when a key repeats the last one is the one
# slurmctld uses, so the last one is the one we take. A commented line never
# counts, except the "#gpuworks-original#" ones we wrote ourselves: they stand
# in for the original lines on a re-run, when those are already commented.
# includes lists the files the text pulls in with Include: what they set is
# not here, and is not ours to edit.
conf_facts() {
	awk -v begin="$BEGIN_MARK" -v end="$END_MARK" -v flag="$EOL_MARK" \
		-v prefix="$ORIG_PREFIX" -v me="$SCRIPT_DIR/check.sh" '
function value(s) {
	sub(/^[^=]*=/, "", s)
	sub(/#.*/, "", s)
	sub(/^[ \t]+/, "", s)
	sub(/[ \t].*/, "", s)
	return s
}
BEGIN { plen = length(prefix) }
$0 == begin { inblk = 1; nblk++; next }
inblk {
	if ($0 == end) { inblk = 0; next }
	if ($0 == flag) noeol = "yes"
	if (tolower($0) ~ /^[ \t]*healthcheckinterval[ \t]*=/) bi = value($0)
	next
}
{
	l = $0
	mk = 0
	if (substr(l, 1, plen) == prefix) { l = substr(l, plen + 1); mk = 1 }
	lc = tolower(l)
	if (lc ~ /^[ \t]*healthcheckprogram[ \t]*=/) {
		v = value(l)
		if (v == "" || v == me) next
		if (mk) mp = v; else op = v
	} else if (lc ~ /^[ \t]*healthcheckinterval[ \t]*=/) {
		v = value(l)
		if (mk) mi = v; else oi = v
	} else if (lc ~ /^[ \t]*healthchecknodestate[ \t]*=/) {
		v = value(l)
		if (mk) ms = v; else os = v
	} else if (!mk && lc ~ /^[ \t]*include[ \t]+/) {
		v = l
		sub(/^[ \t]*[Ii][Nn][Cc][Ll][Uu][Dd][Ee][ \t]+/, "", v)
		sub(/[ \t].*/, "", v)
		inc = (inc == "" ? v : inc "," v)
	}
}
END {
	print "has_block=" (nblk > 0 ? "yes" : "no")
	print "unterminated=" (inblk ? "yes" : "no")
	print "noeol=" (noeol == "yes" ? "yes" : "no")
	print "orig_program=" (op != "" ? op : mp)
	print "orig_interval=" (oi != "" ? oi : mi)
	print "orig_state=" (os != "" ? os : ms)
	print "includes=" inc
	print "block_interval=" bi
}' "$CONF"
}

# conf_rewrite prints slurm.conf without our block. xform=comment also turns
# every active HealthCheck* setting into a "#gpuworks-original#" comment in
# place; xform=uncomment turns those comments back. Doing it in place, rather
# than moving the lines into the block, is what lets --remove give back the
# original file byte for byte. awk cannot see a missing final newline, so it is
# told: a file without our block is checked by the caller, and the newline we
# had to add in front of a block (EOL_MARK) is taken away again with it.
conf_rewrite() {
	plain=no
	if [ -s "$CONF" ] && [ -n "$(tail -c 1 "$CONF")" ]; then
		plain=yes
	fi
	awk -v begin="$BEGIN_MARK" -v end="$END_MARK" -v flag="$EOL_MARK" \
		-v prefix="$ORIG_PREFIX" -v xform="$1" -v plain="$plain" '
BEGIN { plen = length(prefix) }
$0 == begin { inblk = 1; nblk++; next }
inblk {
	if ($0 == flag) sawflag = 1
	if ($0 == end) { inblk = 0; endnr = NR; endflag = sawflag; sawflag = 0 }
	next
}
{
	line = $0
	if (xform == "comment" && tolower(line) ~ /^[ \t]*healthcheck(program|interval|nodestate)[ \t]*=/) {
		line = prefix line
	} else if (xform == "uncomment" && substr(line, 1, plen) == prefix) {
		line = substr(line, plen + 1)
	}
	out[++n] = line
}
END {
	strip = (nblk > 0) ? (endnr == NR && endflag) : (plain == "yes")
	for (i = 1; i <= n; i++) {
		if (i == n && strip) printf "%s", out[i]
		else printf "%s\n", out[i]
	}
}' "$CONF"
}

backup_conf() {
	ts=$(date +%Y%m%d%H%M%S)
	BACKUP="$CONF.gpuworks-$ts.bak"
	i=1
	while [ -e "$BACKUP" ]; do
		BACKUP="$CONF.gpuworks-$ts-$i.bak"
		i=$((i + 1))
	done
	if ! cp -p "$CONF" "$BACKUP"; then
		fail "cannot back up $CONF"
	fi
}

# store_conf replaces slurm.conf's content in place (same inode, so owner,
# mode and a symlink all survive), after a backup.
store_conf() {
	backup_conf
	if ! cat "$1" >"$CONF"; then
		say "ERROR: writing $CONF failed; the backup is $BACKUP."
		fail "cannot write $CONF"
	fi
}

# analyze_conf reads the facts. What the cluster runs now is what slurmctld
# says (`scontrol show config`), not what this file says: the setting may live
# in a file slurm.conf includes, or in a file that was edited and not yet
# reloaded. The text decides only when there is no answer (our own block is
# already in: slurmctld then reports ours, and the original lines are the
# "#gpuworks-original#" comments).
analyze_conf() {
	FACTS=$(conf_facts) || fail "cannot read $CONF"
	if [ "$(fact unterminated)" = yes ]; then
		pre_fail conf_unterminated_block "$CONF has '$BEGIN_MARK' without '$END_MARK'; fix it by hand first."
	fi
	ORIG_PROGRAM=$(fact orig_program)
	ORIG_STATE=$(fact orig_state)
	ORIG_INTERVAL=$(fact orig_interval)
	cfg_program=$(conf_value HealthCheckProgram)
	case "$cfg_program" in '(null)' | none) cfg_program='' ;; esac
	if [ "$(fact has_block)" != yes ] && [ -n "$cfg_program" ] && [ "$cfg_program" != "$SCRIPT_DIR/check.sh" ]; then
		if [ "$cfg_program" != "$ORIG_PROGRAM" ]; then
			PARTIAL=1
			say "WARNING: slurmctld runs the health check program $cfg_program, which $CONF does not name"
			say "(${ORIG_PROGRAM:-no HealthCheckProgram line}): it is set in a file that slurm.conf includes, or $CONF was edited since the last reconfigure."
			say "gpuworks chains $cfg_program, but cannot edit an included file: if the setting stays there it may override ours (checked after the reconfigure)."
			mark "step=conf_source status=warn reason=program_not_in_conf program=$(clean "$cfg_program")"
		fi
		ORIG_PROGRAM=$cfg_program
		ORIG_STATE=$(conf_value HealthCheckNodeState)
		cfg_interval=$(conf_value HealthCheckInterval | awk '{ print $1 }')
		case "$cfg_interval" in '' | *[!0-9]*) ;; *) ORIG_INTERVAL=$cfg_interval ;; esac
	fi
	# Our block is in, and slurm.conf keeps no original line: the original
	# lived where this file does not show it (an included file), and the
	# earlier run recorded it in chained. Keep that, or a re-run would stop
	# the cluster's own health check.
	if [ "$(fact has_block)" = yes ] && [ -z "$ORIG_PROGRAM" ] && [ -s "$SCRIPT_DIR/chained" ]; then
		ORIG_PROGRAM=$(sed -n 1p "$SCRIPT_DIR/chained" | tr -d '\r')
		ORIG_STATE=$(sed -n 2p "$SCRIPT_DIR/chained" | tr -d '\r')
	fi
	INCLUDES=$(fact includes)
	if [ -n "$INCLUDES" ]; then
		say "WARNING: $CONF includes other files ($INCLUDES). Health check settings in them are not seen by the edit below;"
		say "check them if the health check does not take effect (the result is verified after the reconfigure)."
		mark "step=conf_include status=warn files=$(clean "$INCLUDES")"
	fi
	# A state list with only CYCLE (or none) is ANY, plus the cycling.
	ORIG_STATE=$(printf '%s' "${ORIG_STATE:-ANY}" | tr 'a-z' 'A-Z' | tr -cd 'A-Z_,')
	[ -n "$ORIG_STATE" ] || ORIG_STATE=ANY
}

# plan_chain settles which program to chain: the first line of
# <script dir>/chained is the program, the second the node states it runs in
# (the original HealthCheckNodeState). It has to exist before the distribution
# step copies it.
plan_chain() {
	settle_chain
	if [ -n "$CHAINED" ]; then
		tmp="$SCRIPT_DIR/.chained.$$"
		if printf '%s\n%s\n' "$CHAINED" "$ORIG_STATE" >"$tmp" && mv -f "$tmp" "$SCRIPT_DIR/chained"; then
			say "The existing health check program $CHAINED will run after ours and keep its exit status,"
			say "on nodes in the states it asked for ($ORIG_STATE), as before."
			case ",$ORIG_STATE," in
			*,CYCLE,*) say "HealthCheckNodeState keeps CYCLE: slurmctld still spreads the runs over the interval, which now spreads ours too." ;;
			esac
		else
			rm -f "$tmp"
			fail "cannot write $SCRIPT_DIR/chained"
		fi
	else
		# A leftover would revive a program the cluster no longer uses.
		rm -f "$SCRIPT_DIR/chained"
	fi
	mark "step=chained status=ok program=$(clean "${CHAINED:-none}") node_state=$(chained_state)"
}

# settle_chain decides which program is chained, without writing anything: the
# cluster's own, unless that is ours already.
settle_chain() {
	CHAINED=$ORIG_PROGRAM
	if [ "$CHAINED" = "$SCRIPT_DIR/check.sh" ]; then
		CHAINED=''
	fi
}

# chained_state is the node states the chained program runs in, "none" without one.
chained_state() {
	if [ -z "$CHAINED" ]; then
		printf 'none'
	else
		clean "$ORIG_STATE"
	fi
}

# choose_interval settles the health check interval: the cadence of someone's
# health checks is not ours to change, so an explicit --interval wins, then the
# interval they had, then our own earlier choice, then the default. It is
# known before the check script is written: the script spreads its work over
# half of it. BLOCK_STATE is the node state our block asks for: ANY, which the
# install needs, plus CYCLE when the original had it.
choose_interval() {
	INTERVAL=$INTERVAL_OPT
	for v in "$ORIG_INTERVAL" "$(fact block_interval)"; do
		case "$v" in '' | 0 | *[!0-9]*) continue ;; esac
		[ "$v" -gt 0 ] || continue
		[ -n "$INTERVAL" ] || INTERVAL=$v
	done
	[ -n "$INTERVAL" ] || INTERVAL=$DEFAULT_INTERVAL
	BLOCK_STATE=ANY
	case ",$ORIG_STATE," in
	*,CYCLE,*) BLOCK_STATE=ANY,CYCLE ;;
	esac
}

# write_block prints our block. The interval comment explains why the number
# is what it is: other people's health checks must not change cadence.
write_block() {
	printf '%s\n' "$BEGIN_MARK"
	printf '%s\n' '# Managed by gpuworks (the setup script). Remove it with the --remove command from the console.'
	printf '%s\n' '# Any HealthCheck* setting that was here before is kept as a "#gpuworks-original#" comment; its program is run by check.sh,'
	printf '%s\n' '# in the node states its HealthCheckNodeState named. This one is ANY (plus CYCLE when it had it): the install runs in every state.'
	printf '%s\n' '# The interval keeps the cluster existing one, else 300 seconds: an agent that stops is restored by the next run.'
	[ "$1" = yes ] && printf '%s\n' "$EOL_MARK"
	printf '%s\n' "HealthCheckProgram=$SCRIPT_DIR/check.sh"
	printf '%s\n' "HealthCheckInterval=$2"
	printf '%s\n' "HealthCheckNodeState=$3"
	printf '%s\n' "$END_MARK"
}

# build_conf writes the slurm.conf the install would leave to $WORK/new: the
# original with its HealthCheck settings commented in place and our block at
# the end. Nothing is stored.
build_conf() {
	conf_rewrite comment >"$WORK/pristine" || fail "cannot read $CONF"
	noeol=no
	if [ -s "$WORK/pristine" ] && [ -n "$(tail -c 1 "$WORK/pristine")" ]; then
		noeol=yes
	fi
	{
		cat "$WORK/pristine"
		[ "$noeol" = yes ] && printf '\n'
		write_block "$noeol" "$INTERVAL" "$BLOCK_STATE"
	} >"$WORK/new"
}

update_conf() {
	# The cadence of someone's health checks is not ours to change: an
	# explicit --interval wins, then the interval they had, then our own
	# earlier choice, then the default.
	interval=$INTERVAL
	build_conf
	if cmp -s "$CONF" "$WORK/new"; then
		BACKUP=none
		say "$CONF already has the gpuworks block."
	else
		store_conf "$WORK/new"
		say "Updated $CONF (backup: $BACKUP)."
	fi
	say "HealthCheckInterval=$interval"
	mark "step=slurm_conf status=ok path=$(clean "$CONF") backup=$(clean "$BACKUP") chained=$(clean "${CHAINED:-none}") interval=$interval"
}
