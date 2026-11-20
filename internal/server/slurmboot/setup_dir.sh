# The script directory checks: only root may be able to change it.
# (A fragment of the setup script: setup.sh pulls it in where it is needed.)

# DIR_CHECK defines gwks_dir_unsafe, which says whether an account other than
# root could change a directory, or a directory above it: slurmd runs
# check.sh from the script directory as root on every node, so whoever can
# change it runs code as root on all of them. It is text, run here with eval
# and sent as is to the nodes the scripts are copied to, so both check alike;
# it needs only a POSIX shell, stat and readlink, and what it cannot read
# counts as unsafe.
#
# gwks_dir_unsafe <absolute dir> prints nothing when the directory is safe,
# else "<kind>|<path>|<owner>|<mode>|<why>": kind is "open" when the only
# problem is the directory itself being writable by group or others, else
# "bad". The path is followed one name at a time, as the kernel does: every
# directory passed through must be owned by root and writable by neither
# group nor others, except that above the directory itself a sticky one
# (/tmp) is fine, since nobody else can replace what root has there; every
# symbolic link passed through must be root's too (a link in a sticky
# directory belongs to whoever made it), and is followed in turn, at most 40
# of them. A missing tail is fine (root makes it); a dangling link is not. A
# share exported with root squash shows root's files as nobody's.
# GWKS_OWNER_UID stands in for root's uid in tests.
DIR_CHECK='
gwks_dir_bits() {
	gw_b=$1
	while [ ${#gw_b} -lt 4 ]; do gw_b=0$gw_b; done
	case $gw_b in
	[0-7][0-7][0-7][0-7]) ;;
	*) printf "w-"; return 0 ;;
	esac
	case $gw_b in
	??[2367]? | ???[2367]) printf w ;;
	*) printf - ;;
	esac
	case $gw_b in
	[1357]???) printf t ;;
	*) printf - ;;
	esac
}
gwks_root_uid() {
	[ "$1" = 0 ] || [ "$1" = "${GWKS_OWNER_UID:-0}" ]
}
gwks_dir_one() {
	gw_i=$(stat -L -c "%u %a" "$1" 2>/dev/null) || gw_i=""
	gw_u=${gw_i%% *}
	gw_m=${gw_i#* }
	if [ -z "$gw_i" ] || [ -z "$gw_m" ] || [ "$gw_u" = "$gw_i" ]; then
		printf "bad|%s|?|?|its owner and mode cannot be read\n" "$1"
		return 0
	fi
	if ! gwks_root_uid "$gw_u"; then
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
	gw_r=$(gwks_dir_one / bad)
	if [ -n "$gw_r" ]; then printf "%s\n" "$gw_r"; return 0; fi
	gw_cur=""
	gw_rest=${1#/}
	gw_hops=0
	gw_open=""
	while [ -n "$gw_rest" ]; do
		gw_c=${gw_rest%%/*}
		case $gw_rest in */*) gw_rest=${gw_rest#*/} ;; *) gw_rest="" ;; esac
		case $gw_c in
		"" | .) continue ;;
		..) gw_cur=${gw_cur%/*}; continue ;;
		esac
		gw_n=$gw_cur/$gw_c
		if [ -L "$gw_n" ]; then
			gw_hops=$((gw_hops + 1))
			gw_u=$(stat -c %u "$gw_n" 2>/dev/null) || gw_u=""
			gw_t=$(readlink "$gw_n" 2>/dev/null) || gw_t=""
			if [ "$gw_hops" -gt 40 ] || [ -z "$gw_t" ] || [ -z "$gw_u" ]; then
				printf "bad|%s|?|?|its symbolic links cannot be followed\n" "$gw_n"
				return 0
			fi
			if ! gwks_root_uid "$gw_u"; then
				printf "bad|%s|%s|?|it is a symbolic link owned by uid %s, not by root\n" "$gw_n" "$gw_u" "$gw_u"
				return 0
			fi
			if [ ! -e "$gw_n" ]; then
				printf "bad|%s|?|?|it is a dangling symbolic link\n" "$gw_n"
				return 0
			fi
			case $gw_t in /*) gw_cur="" ;; esac
			gw_rest=${gw_t#/}${gw_rest:+/$gw_rest}
			continue
		fi
		[ -e "$gw_n" ] || return 0
		if [ -n "$gw_rest" ]; then
			gw_r=$(gwks_dir_one "$gw_n" bad)
		else
			gw_r=$(gwks_dir_one "$gw_n" open)
		fi
		case $gw_r in
		bad*) printf "%s\n" "$gw_r"; return 0 ;;
		open*) gw_open=$gw_r ;;
		esac
		gw_cur=$gw_n
	done
	[ -n "$gw_open" ] && printf "%s\n" "$gw_open"
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
		# Checked again once closed: something put there before the chmod
		# went through is caught now, as nothing can be added after it.
		if chmod go-w "$path" && own_script_dir && [ -z "$(gwks_dir_unsafe "$SCRIPT_DIR")" ]; then
			say "The script directory $path was writable by group or others ($mode): made it not."
			mark "step=script_dir status=ok action=tightened path=$(clean "$path") mode=$(clean "$mode")"
			return 0
		fi
		why="it was writable by group or others, and closing it did not leave only root's files"
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
