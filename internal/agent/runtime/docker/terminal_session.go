package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sync"
	"time"
)

// ExecPTYSession owns the container-side session as well as its local CLI.
// Docker's exec process is created by the daemon, outside the CLI process tree.
func (d *Driver) ExecPTYSession(ctx context.Context, id string, command []string) ([]string, func(context.Context) error, error) {
	argv, err := d.ExecPTY(ctx, id, command)
	if err != nil {
		return nil, nil, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return nil, nil, err
	}
	directory := "/tmp/gpuworks-webterm-" + hex.EncodeToString(nonce[:])
	// Capture the validated endpoint; a later environment change must not move
	// cleanup to a different daemon. No agent environment enters the container.
	id = argv[6]
	base := slices.Clone(argv[:3])
	invoke := func(ctx context.Context, script string, args ...string) error {
		cli := append(slices.Clone(base[1:]), "exec", id, "/bin/sh", "-c", script, "gpuworks-terminal", directory)
		cli = append(cli, args...)
		output, err := exec.CommandContext(ctx, base[0], cli...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker terminal %s: %w: %s", directory, err, output)
		}
		return nil
	}
	var mu sync.Mutex
	closed := false
	closeSession := func(parent context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return nil
		}
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		defer cancel()
		started := time.Now()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		signal := "HUP"
		for {
			err := invoke(ctx, terminalStatScript+terminalCloseScript, signal)
			if err == nil {
				closed = true
				return nil
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 75 {
				return err
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("docker terminal %s close: %w", directory, ctx.Err())
			case <-ticker.C:
			}
			signal = "NONE"
			if time.Since(started) >= 500*time.Millisecond {
				signal = "KILL"
			}
		}
	}
	prepareCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = invoke(prepareCtx, `set -eu; umask 077; mkdir "$1"; : > "$1/armed"`)
	cancel()
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cleanupErr := closeSession(cleanupCtx)
		cancel()
		return nil, nil, errors.Join(err, cleanupErr)
	}
	// ExecPTY always has [docker --host endpoint exec -i -t id command...].
	wrapped := append(slices.Clone(argv[:7]), "/bin/sh", "-c", terminalStatScript+terminalStartScript, "gpuworks-terminal", directory)
	wrapped = append(wrapped, argv[7:]...)
	return wrapped, closeSession, nil
}

// read_stat returns 1 only for an explicit ENOENT. Unknown/permission failures
// are errors, never proof that an owned process disappeared. Both common cat
// implementations are supported; all values are parsed before any signal.
const terminalStatScript = `
set -eu
read_stat() {
 stat_path="/proc/$1/stat"
 if { IFS= read -r stat_line < "$stat_path"; } 2>/dev/null; then
  case "$stat_line" in *') '*) ;; *) echo 'malformed process stat' >&2; return 2;; esac
  stat_tail=${stat_line##*) }
  set -- $stat_tail
  [ "$#" -ge 20 ] || { echo 'incomplete process stat' >&2; return 2; }
  seen_parent=$2; seen_group=$3; seen_session=$4
  shift 19; seen_start=$1
  for number in "$seen_parent" "$seen_group" "$seen_session" "$seen_start"; do
   case "$number" in ''|*[!0-9]*) echo 'invalid process identity' >&2; return 2;; esac
  done
  return 0
 fi
 if stat_error=$(cat "$stat_path" 2>&1); then
  echo 'process stat read failed without ENOENT' >&2; return 2
 fi
 case "$stat_error" in
  "cat: can't open '$stat_path': No such file or directory"|"cat: $stat_path: No such file or directory") return 1;;
  *) printf '%s\n' "$stat_error" >&2; return 2;;
 esac
}
`

const terminalStartScript = `
dir=$1; shift
[ -f "$dir/armed" ] && [ ! -e "$dir/closed" ] || exit 125
if read_stat "$$"; then :; else exit 125; fi
[ "$seen_group" = "$$" ] && [ "$seen_session" = "$$" ] || { echo 'container terminal is not a private session leader' >&2; exit 125; }
umask 077
printf '%s %s %s %s\n' "$$" "$seen_start" "$seen_group" "$seen_session" > "$dir/pid.tmp"
mv "$dir/pid.tmp" "$dir/pid"
# A close between this final guard and exec is safe: the atomically registered
# PID/start identity already exists, and exec preserves that identity.
[ -f "$dir/armed" ] && [ ! -e "$dir/closed" ] || exit 125
exec "$@"
`

const terminalCloseScript = `
dir=$1; signal=$2
# A failed/partial prepare may have left no directory. Create a closed guard
# before examining registration, so late starts cannot resurrect a session.
mkdir "$dir" 2>/dev/null || [ -d "$dir" ] || exit 2
: > "$dir/closed"
rm -f "$dir/armed"
remove_guard() {
 rm -f "$dir/armed" "$dir/closed" "$dir/pid" "$dir/pid.tmp"
 rmdir "$dir"
}
if record=$(cat "$dir/pid" 2>&1); then :; else
 case "$record" in
  "cat: can't open '$dir/pid': No such file or directory"|"cat: $dir/pid: No such file or directory") remove_guard; exit 0;;
  *) printf '%s\n' "$record" >&2; exit 2;;
 esac
fi
set -- $record
[ "$#" = 4 ] || { echo 'malformed terminal registration' >&2; exit 2; }
owner=$1; owner_start=$2; owner_group=$3; owner_session=$4
for number in "$owner" "$owner_start" "$owner_group" "$owner_session"; do
 case "$number" in ''|*[!0-9]*) echo 'invalid terminal registration' >&2; exit 2;; esac
done
[ "$owner" -gt 1 ] && [ "$owner_group" = "$owner" ] && [ "$owner_session" = "$owner" ] || { echo 'unsafe terminal session identity' >&2; exit 2; }
if read_stat "$owner"; then
 if [ "$seen_start" != "$owner_start" ]; then remove_guard; exit 0; fi
 [ "$seen_group" = "$owner_group" ] && [ "$seen_session" = "$owner_session" ] || { echo 'terminal session identity changed' >&2; exit 2; }
else
 status=$?; [ "$status" = 1 ] || exit "$status"
fi
members=''; others=''; leader=''
for path in /proc/[0-9]*/stat; do
 candidate=${path#/proc/}; candidate=${candidate%/stat}
 [ "$candidate" -gt 1 ] || continue
 if read_stat "$candidate"; then
  identity="$candidate:$seen_start:$seen_session:$seen_parent"
  if [ "$seen_session" != "$owner_session" ]; then
   others="$others $identity"
  elif [ "$candidate" = "$owner" ]; then
   leader="$identity"
  else
   members="$members $identity"
  fi
 else
  status=$?; [ "$status" = 1 ] || exit "$status"
 fi
done
members="$members${leader:+ $leader}"
# Session-changing wrappers (setsid/runuser) remain our descendants even though
# their SID differs. Expand the snapshot before signalling, preserving parents
# until their children disappear so they can reap across these session seams.
while [ -n "$others" ]; do
 remaining=''; added=0
 for other in $others; do
  parent=${other##*:}; owned=0
  for member in $members; do
   if [ "${member%%:*}" = "$parent" ]; then owned=1; break; fi
  done
  if [ "$owned" = 1 ]; then members="$members $other"; added=1; else remaining="$remaining $other"; fi
 done
 [ "$added" = 1 ] || break
 others=$remaining
done
[ -n "$members" ] || { remove_guard; exit 0; }
# Only signal leaves in this snapshot. Merely iterating children before
# parents in one pass still kills a parent before it can wait/reap, leaving
# zombies under an init that does not reap. An extant child (including Z)
# keeps its parent ineligible until a later observation proves it vanished.
for member in $members; do
 candidate=${member%%:*}; remainder=${member#*:}; expected_start=${remainder%%:*}
 remainder=${remainder#*:}; expected_session=${remainder%%:*}
 has_child=0
 for other in $members; do
  if [ "${other##*:}" = "$candidate" ]; then has_child=1; break; fi
 done
 [ "$has_child" = 0 ] || continue
 if read_stat "$candidate"; then
  [ "$seen_start" = "$expected_start" ] && [ "$seen_session" = "$expected_session" ] || continue
  if [ "$signal" != NONE ]; then
   if kill "-$signal" "$candidate"; then :; else
    if read_stat "$candidate"; then
     [ "$seen_start" != "$expected_start" ] || { echo 'terminal signal failed' >&2; exit 2; }
    else
     status=$?; [ "$status" = 1 ] || exit "$status"
    fi
   fi
  fi
 else
  status=$?; [ "$status" = 1 ] || exit "$status"
 fi
done
exit 75
`
