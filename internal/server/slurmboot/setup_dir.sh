# The script directory checks: only root may be able to change it.
# (A fragment of the setup script: setup.sh pulls it in where it is needed.)

# DIR_CHECK defines gwks_dir_unsafe, which says whether an account other than
# root could change a directory, or a directory above it: slurmd runs
# check.sh from the script directory as root on every node, so whoever can
# change it runs code as root on all of them. It is text, run here with eval
# and sent as is to the nodes the scripts are copied to, so both check alike.
#
# gwks_dir_unsafe <absolute dir> prints nothing when the directory is safe,
# else "<kind>|<path>|<owner>|<mode>|<why>": kind is "open" when the only
# problem is the directory itself being writable by group or others, else
# "bad". Every directory on the path counts twice: as written (the parent of a
# symbolic link is where the link can be replaced) and as it resolves (cd -P:
# the directories the link leads through). Each must be owned by root and
# writable by neither group nor others; above the directory itself a sticky
# one (/tmp) is fine, since nobody else can replace what root put in it. A
# missing tail is fine (root makes it); a dangling symbolic link is not. A
# share exported with root squash shows root's files as nobody's.
# GWKS_OWNER_UID stands in for root's uid in tests.
DIR_CHECK='
gwks_dir_bits() {
	printf "%s" "$1" | awk "{ m = \$1; while (length(m) < 4) m = \"0\" m
		s = substr(m, 1, 1) + 0; g = substr(m, 3, 1) + 0; o = substr(m, 4, 1) + 0
		w = (g == 2 || g == 3 || g == 6 || g == 7 || o == 2 || o == 3 || o == 6 || o == 7)
		t = (s == 1 || s == 3 || s == 5 || s == 7)
		printf \"%s%s\", (w ? \"w\" : \"-\"), (t ? \"t\" : \"-\") }"
}
gwks_dir_one() {
	gw_i=$(stat -L -c "%u %a" "$1" 2>/dev/null) || gw_i=""
	gw_u=${gw_i%% *}
	gw_m=${gw_i#* }
	if [ -z "$gw_i" ] || [ -z "$gw_m" ]; then
		printf "bad|%s|?|?|its owner and mode cannot be read\n" "$1"
		return 0
	fi
	if [ "$gw_u" != 0 ] && [ "$gw_u" != "${GWKS_OWNER_UID:-0}" ]; then
		printf "bad|%s|%s|%s|it is owned by uid %s, not by root\n" "$1" "$gw_u" "$gw_m" "$gw_u"
		return 0
	fi
	case $(gwks_dir_bits "$gw_m") in
	w-) printf "%s|%s|%s|%s|it is writable by group or others\n" "$2" "$1" "$gw_u" "$gw_m" ;;
	wt) [ "$2" = open ] && printf "open|%s|%s|%s|it is writable by group or others\n" "$1" "$gw_u" "$gw_m" ;;
	esac
	return 0
}
gwks_dir_unsafe() {
	case $1 in
	/*) ;;
	*) printf "bad|%s|?|?|it is not an absolute path\n" "$1"; return 0 ;;
	esac
	gw_e=$1
	while [ ! -e "$gw_e" ] && [ ! -L "$gw_e" ]; do gw_e=$(dirname "$gw_e"); done
	if [ -L "$gw_e" ] && [ ! -e "$gw_e" ]; then
		printf "bad|%s|?|?|it is a dangling symbolic link\n" "$gw_e"
		return 0
	fi
	gw_p=$(cd -P "$gw_e" 2>/dev/null && pwd -P) || gw_p=""
	if [ -z "$gw_p" ]; then
		printf "bad|%s|?|?|it cannot be resolved\n" "$gw_e"
		return 0
	fi
	gw_bad=""
	for gw_d in "$gw_e" "$gw_p"; do
		gw_leaf=no
		[ "$gw_e" = "$1" ] && gw_leaf=yes
		while :; do
			if [ ! -L "$gw_d" ]; then
				if [ "$gw_leaf" = yes ]; then
					gw_r=$(gwks_dir_one "$gw_d" open)
				else
					gw_r=$(gwks_dir_one "$gw_d" bad)
				fi
				case $gw_r in
				bad*) printf "%s\n" "$gw_r"; return 0 ;;
				open*) gw_bad=$gw_r ;;
				esac
			fi
			gw_leaf=no
			[ "$gw_d" = / ] && break
			gw_d=$(dirname "$gw_d")
		done
	done
	[ -n "$gw_bad" ] && printf "%s\n" "$gw_bad"
	return 0
}
'
eval "$DIR_CHECK"

# check_script_dir refuses a script directory an account other than root
# could change (gwks_dir_unsafe), before anything is changed. One that is
# only itself too open is the setup's own when everything in it is root's
# plain files, none writable by others (anything else may have been put there
# while it was open): it is tightened once the whole path is known safe, and
# in check mode only reported. Anything else is refused, with what to do.
# $1 "warn" (removing the setup) reports instead of refusing.
check_script_dir() {
	r=$(gwks_dir_unsafe "$SCRIPT_DIR")
	if [ -z "$r" ]; then
		mark "step=script_dir status=ok path=$(clean "$SCRIPT_DIR")"
		return 0
	fi
	kind=${r%%|*}
	rest=${r#*|}
	path=${rest%%|*}
	rest=${rest#*|}
	owner=${rest%%|*}
	rest=${rest#*|}
	mode=${rest%%|*}
	why=${rest#*|}
	if [ "${1:-}" = warn ]; then
		say "WARNING: $path is not safe for the health check: $why. Fix it before connecting the cluster again."
		mark "step=script_dir status=warn path=$(clean "$path") owner=$(clean "$owner") mode=$(clean "$mode")"
		return 0
	fi
	if [ "$kind" = open ] && own_script_dir; then
		if [ "$CHECK_ONLY" = yes ]; then
			say "The script directory $path is writable by group or others ($mode); applying makes it not."
			mark "step=script_dir status=plan action=tighten path=$(clean "$path") mode=$(clean "$mode")"
			return 0
		fi
		if chmod go-w "$path" && [ -z "$(gwks_dir_unsafe "$SCRIPT_DIR")" ]; then
			say "The script directory $path was writable by group or others ($mode): made it not."
			mark "step=script_dir status=ok action=tightened path=$(clean "$path") mode=$(clean "$mode")"
			return 0
		fi
		why="it is writable by group or others and could not be changed"
	fi
	script_dir_unsafe "$path" "$owner" "$mode" "$why"
}

# own_script_dir says whether the script directory holds what an earlier
# setup left and nothing else anyone could have put there: at least one of
# its files, and every entry a plain file of root's that group and others
# cannot write (no link, no directory, no file of another account).
own_script_dir() {
	[ -f "$SCRIPT_DIR/check.sh" ] || [ -f "$SCRIPT_DIR/token" ] || [ -f "$SCRIPT_DIR/chained" ] || return 1
	for e in "$SCRIPT_DIR"/* "$SCRIPT_DIR"/.[!.]* "$SCRIPT_DIR"/..?*; do
		[ -e "$e" ] || [ -L "$e" ] || continue
		[ -f "$e" ] && [ ! -L "$e" ] || return 1
		i=$(stat -c '%u %a' "$e" 2>/dev/null) || return 1
		[ "${i%% *}" = 0 ] || [ "${i%% *}" = "${GWKS_OWNER_UID:-0}" ] || return 1
		case $(gwks_dir_bits "${i#* }") in w*) return 1 ;; esac
	done
	return 0
}

# script_dir_unsafe refuses the setup over a directory on the script
# directory's path: $1 the directory, $2 its owner, $3 its mode, $4 why.
script_dir_unsafe() {
	say "slurmd runs $SCRIPT_DIR/check.sh as root on every node, so only root may be able to change it or any directory above it."
	say "Make $1 owned by root and not writable by group or others (chown root $1; chmod go-w $1), or choose another script directory."
	mark "step=script_dir status=failed path=$(clean "$1") owner=$(clean "$2") mode=$(clean "$3")"
	pre_fail script_dir_unsafe "$1 is not safe for the health check: $4."
}
