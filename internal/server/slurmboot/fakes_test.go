package slurmboot

// Fakes of the tools the scripts call. Each one is a small shell script that
// behaves like the real tool for the invocations the scripts make and rejects
// anything else the way the real tool would: a stub that answers everything
// with success would make the tests prove nothing. State lives in the
// directory named by GWKS_FAKE_STATE, so a test arranges the world by
// creating files there and reads what happened from the call logs.

var fakeTools = map[string]string{
	"id": `#!/bin/sh
if [ "$*" = "-u" ]; then
	echo "${FAKE_UID:-0}"
	exit 0
fi
echo "id: unsupported arguments: $*" >&2
exit 1
`,

	"hostname": `#!/bin/sh
full=${FAKE_HOSTNAME:-cn000.cluster.local}
case "$1" in
-s) echo "${full%%.*}" ;;
"") echo "$full" ;;
*) echo "hostname: unsupported arguments: $*" >&2; exit 1 ;;
esac
`,

	"chown": `#!/bin/sh
echo "chown $*" >> "$GWKS_FAKE_STATE/chown"
`,

	"scontrol": `#!/bin/sh
S=$GWKS_FAKE_STATE
echo "scontrol $*" >> "$S/calls"
if [ -e "$S/scontrol-down" ]; then
	echo "scontrol: error: slurm_load_ctl_conf: Unable to contact slurm controller (connect failure)" >&2
	exit 1
fi
case "$1" in
show)
	if [ "$2" != config ]; then
		echo "scontrol: unsupported: $*" >&2
		exit 1
	fi
	conf=${FAKE_SLURM_CONF:-/etc/slurm/slurm.conf}
	fh=${FAKE_HOSTNAME:-cn000.cluster.local}
	hc=$(sed -n 's/^HealthCheckProgram=//p' "$conf" 2>/dev/null | tail -n 1)
	[ -n "$hc" ] || hc=${FAKE_ORIG_PROGRAM:-}
	[ -n "$hc" ] || hc='(null)'
	# a controller that has not taken the new file (or an included file wins)
	if [ -e "$S/config-frozen" ]; then
		hc=${FAKE_ORIG_PROGRAM:-'(null)'}
	fi
	hn=$(sed -n 's/^HealthCheckNodeState=//p' "$conf" 2>/dev/null | tail -n 1)
	[ -n "$hn" ] || hn=${FAKE_ORIG_STATE:-ANY}
	hi=$(sed -n 's/^HealthCheckInterval=//p' "$conf" 2>/dev/null | tail -n 1)
	[ -n "$hi" ] || hi=${FAKE_ORIG_INTERVAL:-0}
	cat <<EOF
Configuration data as of 2026-11-16T10:00:00
AccountingStorageBackupHost = (null)
AuthType                = auth/munge
ClusterName             = ${FAKE_CLUSTER:-demo}
HealthCheckInterval     = $hi sec
HealthCheckNodeState    = $hn
HealthCheckProgram      = $hc
SLURM_CONF              = $conf
SLURM_VERSION           = 22.05.8
SlurmctldHost[0]        = ${FAKE_CTLD_HOST:-${fh%%.*}}(10.0.0.1)
SlurmctldParameters     = ${FAKE_CTLD_PARAMS:-enable_configless}
EOF
	;;
reconfigure)
	if [ -e "$S/reconfigure-fail" ]; then
		echo "scontrol_reconfigure error: Unable to contact slurm controller (connect failure)" >&2
		exit 1
	fi
	echo reconfigure >> "$S/reconfigured"
	;;
ping)
	echo "Slurmctld(primary) at ctl is UP"
	;;
-o)
	if [ "$2 $3" != "show node" ]; then
		echo "scontrol: unsupported: $*" >&2
		exit 1
	fi
	# no name: every node, from the test's own listing (one line per node)
	if [ -z "${4:-}" ]; then
		cat "$S/node-list" 2>/dev/null
		exit 0
	fi
	if grep -qx "$4" "$S/nodes" 2>/dev/null; then
		echo "NodeName=$4 Arch=x86_64 CoresPerSocket=8 CPUAlloc=0 CPUEfctv=16 CPUTot=16 CPULoad=0.01 AvailableFeatures=(null) ActiveFeatures=(null) Gres=gpu:1 NodeAddr=$4 NodeHostName=$4 Version=22.05.8 OS=Linux State=$(cat "$S/node-state" 2>/dev/null || echo IDLE) ThreadsPerCore=2 Partitions=gpu"
	else
		echo "Node $4 not found" >&2
		exit 1
	fi
	;;
*)
	echo "scontrol: unsupported: $*" >&2
	exit 1
	;;
esac
`,

	"sinfo": `#!/bin/sh
if [ "$*" != "-h -a -N -o %N" ]; then
	echo "sinfo: error: unsupported arguments: $*" >&2
	exit 1
fi
cat "$GWKS_FAKE_STATE/sinfo"
`,

	"nvidia-smi": `#!/bin/sh
if [ "$*" != "-L" ]; then
	echo "nvidia-smi: unsupported arguments: $*" >&2
	exit 1
fi
if [ -e "$GWKS_FAKE_STATE/gpu" ]; then
	echo "GPU 0: Tesla T4 (UUID: GPU-5a3c1e2e-6f6e-4b57-9b0a-0d3d0f6a1c11)"
	exit 0
fi
echo "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver. Make sure that the latest NVIDIA driver is installed and running." >&2
exit 9
`,

	"systemctl": `#!/bin/sh
S=$GWKS_FAKE_STATE
case "$1" in
is-active)
	shift
	quiet=0
	if [ "${1:-}" = --quiet ]; then
		quiet=1
		shift
	fi
	if [ "${1:-}" != gpuworks-agent ]; then
		echo "inactive"
		exit 3
	fi
	if [ -e "$S/agent-active" ]; then
		[ "$quiet" = 1 ] || echo active
		exit 0
	fi
	[ "$quiet" = 1 ] || echo inactive
	exit 3
	;;
cat)
	if [ "${2:-}" = gpuworks-agent ] && [ -e "$S/agent-unit" ]; then
		printf '# /etc/systemd/system/gpuworks-agent.service\n[Unit]\nDescription=gpuworks agent\n[Service]\nExecStart=%s/gpuworks-agent run --state %s\n' "${FAKE_UNIT_STATE:-/var/lib/gpuworks}" "${FAKE_UNIT_STATE:-/var/lib/gpuworks}"
		exit 0
	fi
	echo "No files found for ${2:-}.service." >&2
	exit 1
	;;
disable | daemon-reload | enable | restart | stop)
	echo "systemctl $*" >> "$S/systemctl"
	[ "$1" = disable ] && rm -f "$S/agent-active"
	[ "$1" = restart ] && : > "$S/agent-active"
	exit 0
	;;
*)
	echo "Unknown command verb $1." >&2
	exit 1
	;;
esac
`,

	// curl serves the installer and answers the token-status question. The
	// question must come with its URL on standard input (curl -K -), never on
	// the argument list, which every user can read. The installer is written
	// to the file -o names, so the caller sees curl's own status. A test that
	// puts the real install.sh in $GWKS_FAKE_STATE/real-installer gets that;
	// otherwise the installer is a stand-in that enforces what the real one
	// does of the Slurm enrollment: the token and the node arrive in the
	// environment, under the names install.sh reads, and a missing token is
	// the real installer's refusal.
	"curl": `#!/bin/sh
S=$GWKS_FAKE_STATE
echo "curl $*" >> "$S/curl"
if [ "$*" = "-fsS --max-time 15 -K -" ]; then
	cfg=$(cat)
	echo "$cfg" >> "$S/token-status-requests"
	if [ -e "$S/token-status-down" ]; then
		echo "curl: (7) Failed to connect" >&2
		exit 7
	fi
	want="url = \"${FAKE_SERVER:-https://gw.example:8800}/slurm/token-status?token=$FAKE_TOKEN\""
	if [ "$cfg" != "$want" ]; then
		echo "curl: (22) The requested URL returned error: 404" >&2
		exit 22
	fi
	if [ -e "$S/token-status" ]; then
		cat "$S/token-status"
	else
		echo '{"status":"active"}'
	fi
	exit 0
fi
# The reachability question (setup.sh: curl -fsS --max-time N -o /dev/null URL/healthz):
# FAKE_HEALTHZ lists "host=curl-exit-status" for the machines that cannot reach
# the server; FAKE_NODE names the machine a command runs on (the fake ssh sets it).
if [ "$1" = "-fsS" ] && [ "$2" = "--max-time" ] && [ "$4" = "-o" ] && [ "$5" = "/dev/null" ] && [ "$#" -eq 6 ]; then
	case "$6" in
	"${FAKE_SERVER:-https://gw.example:8800}/healthz") ;;
	*) echo "curl: (22) The requested URL returned error: 404" >&2; exit 22 ;;
	esac
	echo "healthz ${FAKE_NODE:-ctl} $6" >> "$S/healthz"
	for e in $FAKE_HEALTHZ; do
		if [ "${e%%=*}" = "${FAKE_NODE:-ctl}" ]; then
			echo "curl: (${e#*=}) simulated failure" >&2
			exit "${e#*=}"
		fi
	done
	exit 0
fi
# The agent binary the real installer downloads (-fsSL URL -o FILE).
case "$2" in
*/dl/gpuworks-agent-linux-*)
	if [ "$1" = "-fsSL" ] && [ "$3" = "-o" ] && [ "$#" -eq 4 ] && [ -e "$S/agent-binary" ]; then
		cat "$S/agent-binary" > "$4"
		exit 0
	fi
	echo "curl: (22) The requested URL returned error: 404" >&2
	exit 22
	;;
esac
if [ "$1" != "-fsSL" ] || [ "$2" != "--max-time" ] || [ "$4" != "${FAKE_SERVER:-https://gw.example:8800}/install.sh" ] || [ "$5" != "-o" ] || [ "$#" -ne 6 ]; then
	echo "curl: unsupported arguments: $*" >&2
	exit 2
fi
if [ -e "$S/installer-404" ]; then
	echo "curl: (22) The requested URL returned error: 404" >&2
	exit 22
fi
if [ -e "$S/real-installer" ]; then
	cat "$S/real-installer" > "$6"
	exit 0
fi
cat > "$6" <<'INSTALLER'
S=$GWKS_FAKE_STATE
{
	echo "--- install"
	echo "argc=$#"
	for a in "$@"; do echo "arg=$a"; done
	echo "token=${GPUWORKS_SLURM_ENROLL_TOKEN-unset}"
	echo "node=${GPUWORKS_SLURM_NODE-unset}"
} >> "$S/installer"
if [ -z "${GPUWORKS_SLURM_ENROLL_TOKEN:-}" ]; then
	echo "gpuworks: --token is required; create a node in the UI to obtain an install command" >&2
	exit 2
fi
if [ -e "$S/installer-fails" ]; then
	echo "gpuworks: boom" >&2
	exit 7
fi
sleep "${FAKE_INSTALL_SLEEP:-0}"
if [ -n "${FAKE_INSTALL_ACTIVATES:-}" ]; then
	: > "$S/agent-active"
fi
echo done >> "$S/installer-done"
INSTALLER
`,

	// install puts a file in place the way install(1) does (a name that is a
	// link is replaced, never written through) and records -m and -o; the
	// owner is not applied, a test does not run as root.
	"install": `#!/bin/sh
S=$GWKS_FAKE_STATE
echo "install $*" >> "$S/install"
mode=''
while [ "$#" -gt 2 ]; do
	case "$1" in
	-m) mode=$2; shift 2 ;;
	-o) shift 2 ;;
	*) echo "install: unsupported option $1" >&2; exit 1 ;;
	esac
done
[ -n "$mode" ] || { echo "install: the mode must be given" >&2; exit 1; }
rm -f "$2" || exit 1
cp "$1" "$2" && chmod "$mode" "$2"
`,

	// slurmd prints the card list on stderr, like the real one.
	"slurmd": `#!/bin/sh
S=$GWKS_FAKE_STATE
echo "slurmd $*" >> "$S/slurmd"
if [ "${1:-}" != "-G" ]; then
	echo "slurmd: unsupported arguments: $*" >&2
	exit 1
fi
if [ -e "$S/slurmd-fail" ]; then
	echo "slurmd: error: Unable to open /etc/slurm/slurm.conf: No such file or directory" >&2
	exit 1
fi
echo "slurmd: Gres Name=gpu Type=(null) Count=1 ID=7696487 File=/dev/nvidia0 Links=(null) Flags=HAS_FILE" >&2
echo "slurmd: Gres Name=gpu Type=(null) Count=1 ID=7696487 File=/dev/nvidia1 Links=(null) Flags=HAS_FILE" >&2
`,

	"findmnt": `#!/bin/sh
if [ "$#" -ne 5 ] || [ "$1 $2 $3 $4" != "-n -o FSTYPE -T" ]; then
	echo "findmnt: unsupported arguments: $*" >&2
	exit 1
fi
[ -n "${FAKE_FINDMNT:-}" ] || exit 1
echo "$FAKE_FINDMNT"
`,

	// stat answers the filesystem question; everything else is the real tool.
	"stat": `#!/bin/sh
if [ "$1 $2 $3" = "-f -c %T" ]; then
	echo "${FAKE_STAT_FSTYPE:-UNKNOWN (0x1021994)}"
	exit 0
fi
exec /usr/bin/stat "$@"
`,

	"df": `#!/bin/sh
if [ "$1" != "-PT" ]; then
	echo "df: unsupported arguments: $*" >&2
	exit 1
fi
echo "Filesystem     Type 1024-blocks    Used Available Capacity Mounted on"
echo "fake           ${FAKE_DF_FSTYPE:-ext4} 1000 100 900 10% /"
`,

	// Remote access: paths of the script directory live under a per-node
	// directory, and FAKE_FAIL_NODES lists nodes that cannot be reached.
	"ssh": `#!/bin/sh
S=$GWKS_FAKE_STATE
echo "ssh $*" >> "$S/calls"
batch=0
while [ "$#" -gt 0 ]; do
	case "$1" in
	-o) case "$2" in BatchMode=yes) batch=1 ;; esac; shift 2 ;;
	-*) echo "ssh: unsupported option $1" >&2; exit 255 ;;
	*) break ;;
	esac
done
[ "$batch" = 1 ] || { echo "ssh: missing BatchMode=yes: would prompt" >&2; exit 255; }
node=$1
shift
for f in $FAKE_FAIL_NODES; do
	if [ "$f" = "$node" ]; then
		echo "ssh: connect to host $node port 22: Connection refused" >&2
		exit 255
	fi
done
cmd=$(printf '%s' "$*" | sed "s|$FAKE_SCRIPT_DIR|$S/remote/$node$FAKE_SCRIPT_DIR|g")
echo "start $node" >> "$S/ssh-events"
sleep "${FAKE_SSH_SLEEP:-0}"
FAKE_NODE=$node sh -c "$cmd"
rc=$?
echo "end $node" >> "$S/ssh-events"
exit $rc
`,

	"scp": `#!/bin/sh
S=$GWKS_FAKE_STATE
echo "scp $*" >> "$S/calls"
batch=0
while [ "$#" -gt 0 ]; do
	case "$1" in
	-q) shift ;;
	-o) case "$2" in BatchMode=yes) batch=1 ;; esac; shift 2 ;;
	-*) echo "scp: unsupported option $1" >&2; exit 1 ;;
	*) break ;;
	esac
done
[ "$batch" = 1 ] || { echo "scp: missing BatchMode=yes" >&2; exit 1; }
src=$1
node=${2%%:*}
dest=${2#*:}
for f in $FAKE_FAIL_NODES; do
	if [ "$f" = "$node" ]; then
		echo "ssh: connect to host $node port 22: Connection refused" >&2
		exit 255
	fi
done
for f in $FAKE_SCP_FAIL_NODES; do
	if [ "$f" = "$node" ]; then
		echo "scp: $dest: Permission denied" >&2
		exit 1
	fi
done
echo "start $node" >> "$S/ssh-events"
sleep "${FAKE_SSH_SLEEP:-0}"
cp "$src" "$S/remote/$node$dest"
rc=$?
echo "end $node" >> "$S/ssh-events"
exit $rc
`,

	"pdsh": `#!/bin/sh
S=$GWKS_FAKE_STATE
echo "pdsh $*" >> "$S/calls"
if [ "$1" != "-w" ]; then
	echo "pdsh: unsupported arguments: $*" >&2
	exit 1
fi
nodes=$(echo "$2" | tr ',' ' ')
shift 2
rc=0
for node in $nodes; do
	down=0
	for f in $FAKE_FAIL_NODES; do [ "$f" = "$node" ] && down=1; done
	if [ "$down" = 1 ]; then
		echo "pdsh@ctl: $node: connect: Connection refused" >&2
		rc=1
		continue
	fi
	# like pdsh, expand %u (remote user), %h (host), %n (rank) and %%
	cmd=$(printf '%s' "$*" | sed "s/%%/@GWKS_PCT@/g; s/%u/root/g; s/%h/$node/g; s/%n/0/g; s/@GWKS_PCT@/%/g")
	cmd=$(printf '%s' "$cmd" | sed "s|$FAKE_SCRIPT_DIR|$S/remote/$node$FAKE_SCRIPT_DIR|g")
	FAKE_NODE=$node sh -c "$cmd" 2>&1 | sed "s|^|$node: |"
done
exit $rc
`,

	"pdcp": `#!/bin/sh
S=$GWKS_FAKE_STATE
echo "pdcp $*" >> "$S/calls"
if [ "$1" != "-w" ]; then
	echo "pdcp: unsupported arguments: $*" >&2
	exit 1
fi
nodes=$(echo "$2" | tr ',' ' ')
shift 2
# the last argument is the destination directory, the rest are sources
dest=''
for a in "$@"; do dest=$a; done
rc=0
for node in $nodes; do
	down=0
	for f in $FAKE_FAIL_NODES; do [ "$f" = "$node" ] && down=1; done
	if [ "$down" = 1 ]; then
		echo "pdcp@ctl: $node: connect: Connection refused" >&2
		rc=1
		continue
	fi
	n=$#
	i=1
	for a in "$@"; do
		[ "$i" -lt "$n" ] && cp "$a" "$S/remote/$node$dest" 2>/dev/null
		i=$((i + 1))
	done
done
exit $rc
`,

	"clush": `#!/bin/sh
S=$GWKS_FAKE_STATE
echo "clush $*" >> "$S/calls"
if [ "$1" != "-w" ]; then
	echo "clush: unsupported arguments: $*" >&2
	exit 1
fi
nodes=$(echo "$2" | tr ',' ' ')
shift 2
copy=''
dest=''
case "$1" in
--copy)
	copy=$2
	[ "$3" = "--dest" ] || { echo "clush: --copy needs --dest" >&2; exit 1; }
	dest=$4
	;;
esac
rc=0
for node in $nodes; do
	down=0
	for f in $FAKE_FAIL_NODES; do [ "$f" = "$node" ] && down=1; done
	if [ "$down" = 1 ]; then
		echo "clush: $node: exited with exit code 255" >&2
		rc=1
		continue
	fi
	if [ -n "$copy" ]; then
		cp "$copy" "$S/remote/$node$dest"
	else
		cmd=$(printf '%s' "$*" | sed "s|$FAKE_SCRIPT_DIR|$S/remote/$node$FAKE_SCRIPT_DIR|g")
		FAKE_NODE=$node sh -c "$cmd" 2>&1 | sed "s|^|$node: |"
	fi
done
exit $rc
`,
}

// realTools are linked from the system into the fake PATH.
var realTools = []string{
	"cat", "cp", "mv", "rm", "mkdir", "chmod", "ls", "sed", "awk", "grep", "tr",
	"head", "tail", "sort", "uniq", "date", "wc", "cut", "tee", "ln", "touch",
	"flock", "timeout", "setsid", "nohup", "sleep", "env", "mktemp", "cmp",
	"dirname", "basename", "printf", "readlink", "diff", "true", "false", "od", "cksum", "uname",
}
