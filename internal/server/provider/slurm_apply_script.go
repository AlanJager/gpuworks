package provider

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// What the platform runs on a Slurm cluster's manager node, and how it reads
// what came back.

// SlurmGRESConfPath is where the Slurm controller reads its GRES
// configuration from (the cluster is configless: slurmd fetches it).
const SlurmGRESConfPath = "/etc/slurm/gres.conf"

// The steps of one apply, as the answer names them (the order they run in).
const (
	SlurmApplyIDGres        = "gres"
	SlurmApplyIDCheckScript = "check_script"
	SlurmApplyIDDistribute  = "distribute"
	SlurmApplyIDSlurmConf   = "slurm_conf"
	SlurmApplyIDReconfigure = "reconfigure"
	SlurmApplyIDResume      = "resume"
	// SlurmApplyIDReach is whether the server's address could be reached from
	// the controller and the compute nodes (SlurmReach); it is added once the
	// nodes were asked (SlurmApplyResult.SetReach).
	SlurmApplyIDReach = "reach"
)

// The modes of a run on the manager node.
const (
	// SlurmModeSetup puts the entry in place: gres lines, the setup script
	// (check script, slurm.conf, reconfigure), then resumes drained nodes.
	SlurmModeSetup = "setup"
	// SlurmModeCheck says what SlurmModeSetup would do and changes nothing:
	// no gres.conf line, no file, no copy, no reconfigure, no resume.
	SlurmModeCheck = "check"
	// SlurmModeUninstallAgents arms the uninstall mode of the health check.
	SlurmModeUninstallAgents = "uninstall_agents"
	// SlurmModeRemoveCheck takes the health check out of slurm.conf again.
	SlurmModeRemoveCheck = "remove_check"
)

// SlurmApplyRequest is one run on the manager node. SetupURL is where the
// entry's setup script is served: a one-time URL, so that the command the
// cloud keeps a record of holds no enrollment token. ExpectGPU are the
// compute nodes the cloud says have a card (SlurmModeSetup and SlurmModeCheck):
// the nodes that get a gres.conf line are the ones Slurm itself lists with a
// gpu Gres, so the list only lets a node Slurm gives no gpu Gres be said
// (SlurmApplyResult.NoGPUGres); it may be empty or wrong without harm.
//
// ServerURL is the entry's base URL, which the platform asks the compute nodes
// to reach itself (SlurmNodeProber) when the driver can; it may be empty to ask
// none. SkipNodeReach tells the setup script to ask only the controller,
// because the platform asks the nodes.
type SlurmApplyRequest struct {
	Mode          string
	SetupURL      string
	ExpectGPU     []string
	ServerURL     string
	SkipNodeReach bool
}

// Validate refuses a request the script could not carry out as written.
func (r SlurmApplyRequest) Validate() error {
	switch r.Mode {
	case SlurmModeSetup, SlurmModeCheck, SlurmModeUninstallAgents, SlurmModeRemoveCheck:
	default:
		return fmt.Errorf("unknown Slurm apply mode %q", r.Mode)
	}
	u, err := url.Parse(r.SetupURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(r.SetupURL, "\"'\\\n\r $`") {
		return errors.New("the setup script URL is not a plain http(s) URL")
	}
	for _, n := range r.ExpectGPU {
		if !slurmNodeName.MatchString(n) {
			return fmt.Errorf("slurm node name %q cannot be carried to the manager node", n)
		}
	}
	if r.ServerURL != "" {
		if _, err := SlurmReachProbeScript(r.ServerURL); err != nil {
			return err
		}
	}
	return nil
}

// slurmManagerScript is run as root on the cluster's manager node. It is safe
// to run twice and changes nothing it should not:
//
//   - one run at a time (flock): a second says busy.
//   - gres.conf: the nodes and their cards come from Slurm itself, not from
//     the cloud's instance list: `scontrol -o show node` gives each node's
//     configured Gres= (E-HPC sets it when slurmd registers, so it is there
//     before the node has a gres.conf line, before any sync and for nodes
//     whose names the cloud lists otherwise). A node whose Gres= lists gpu:N
//     (also typed, "gpu:a100:2", and with a socket suffix, "gpu:2(S:0-1)")
//     that has no gpu line (no gpu line whose NodeName names it, wherever in
//     the line it stands or as an expression, and no gpu line without
//     NodeName, which applies to every node) gets one appended for N cards;
//     every other node's lines are left as they are. A node the cloud says has
//     a card (expect_gpu) while Slurm gives it no gpu GRES gets nothing and is
//     reported (gres=nogres). The file is created 0644 when missing, and a last
//     line without a newline gets one first.
//   - the setup script is fetched (the URL goes to curl on its standard input,
//     not its argument list) and run; it prints its own GWKS_SLURM markers,
//     does scontrol reconfigure, and its exit status is the script's.
//   - nodes still held for the missing GPUs (their Slurm Reason begins with
//     the missing-GPU text, as Slurm lists them) are resumed; an
//     administrator's own drain is never lifted.
//   - mode check does the same reading and none of the writing: it prints
//     "gres=would_add" where setup would append a line, runs the setup script
//     with --check (which writes, copies and reconfigures nothing) and prints
//     "resume=would" for the nodes setup would resume.
//   - the setup script asks whether the server's address can be reached from
//     the controller, and from each GPU node over ssh unless reach_flag is
//     --no-node-reach (the platform asks the nodes itself).
//
// Markers are lines "GWKS_APPLY key=value ...". Exit codes: 20 busy, 21 the
// setup script could not be fetched, 11 gres.conf unreadable or unwritable,
// else setup's own. The server reads the markers, not the exit code. The
// placeholders are replaced by SlurmManagerScript with shell-quoted values.
const slurmManagerScript = `exec 2>&1
set -f
PATH="$PATH:/usr/local/sbin:/usr/local/bin:/opt/slurm/bin:/usr/local/slurm/bin"
mode=@MODE@
setup_url=@URL@
gres=@GRES@
expect_gpu=@EXPECT@
reach_flag=@REACHFLAG@
marker() { printf 'GWKS_APPLY %s\n' "$*"; }

lock=/run/gpuworks-slurm-apply.lock
( : >> "$lock" ) 2>/dev/null || lock=/tmp/gpuworks-slurm-apply.lock
if command -v flock >/dev/null 2>&1; then
  exec 9>>"$lock"
  if ! flock -n 9; then
    marker busy=1
    echo "another apply is running on this manager node"
    exit 20
  fi
fi

# covered: 0 when gres.conf already gives node $1 its cards. A line counts only
# when it is a gpu line (Name=gpu, or AutoDetect that is not off) and either
# has no NodeName (it applies to every node) or names the node in its NodeName
# expression, wherever in the line the NodeName is. A line for another kind of
# resource, or for other nodes, covers nothing.
covered() {
  [ -f "$gres" ] || return 1
  exprs=$(awk '
    /^[[:space:]]*(#|$)/ { next }
    {
      gpu = 0; node = ""
      for (i = 1; i <= NF; i++) {
        f = $i; l = tolower(f)
        if (substr(f, 1, 1) == "#") break
        if (l == "name=gpu") gpu = 1
        else if (l ~ /^autodetect=/ && l != "autodetect=off") gpu = 1
        else if (l ~ /^nodename=/) node = substr(f, 10)
      }
      if (!gpu) next
      if (node == "") print "*"; else print node
    }' "$gres") || return 1
  for e in $exprs; do
    case "$e" in
      '*') return 0 ;;
      "$1") return 0 ;;
      *'['* | *,*) if scontrol show hostnames "$e" 2>/dev/null | grep -qx "$1"; then return 0; fi ;;
    esac
  done
  return 1
}

# slurm_nodes prints one line "name cards held" per node Slurm lists: cards is
# what its configured Gres= gives it in gpu (0 for none, "(null)" or another
# resource), held is 1 when its Reason is the missing-GPU one. Typed
# ("gpu:a100:2") and socket-suffixed ("gpu:2(S:0-1)") entries count, several
# gpu entries add up. A name the scripts could not carry is left out.
slurm_nodes() {
  scontrol -o show node 2>/dev/null </dev/null | awk '
    {
      name = ""; gres = ""
      held = index($0, "Reason=gres/gpu count reported lower than configured") > 0 ? 1 : 0
      for (i = 1; i <= NF; i++) {
        f = $i
        if (f ~ /^Reason=/) break
        if (f ~ /^NodeName=/ && name == "") name = substr(f, 10)
        else if (f ~ /^Gres=/ && gres == "") gres = substr(f, 6)
      }
      if (name !~ /^[A-Za-z0-9._-]+$/) next
      gsub(/\([^)]*\)/, "", gres)
      cards = 0
      k = split(gres, items, ",")
      for (j = 1; j <= k; j++) {
        m = split(items[j], p, ":")
        if (p[1] != "gpu") continue
        c = 1
        if (m >= 2 && p[m] ~ /^[0-9]+$/) c = p[m] + 0
        cards += c
      }
      print name, cards, held
    }' | sort
}

add_gres_lines() {
  listing=$(slurm_nodes)
  if printf '%s\n' "$listing" | awk '$2 > 0 { found = 1 } END { exit !found }'; then
    if [ -e "$gres" ] && [ ! -r "$gres" ]; then
      marker gres=error
      echo "cannot read $gres"
      exit 11
    fi
    while read -r n c h; do
      [ "$c" -gt 0 ] 2>/dev/null || continue
      if covered "$n"; then
        marker "gres=present node=$n"
        continue
      fi
      files=/dev/nvidia0
      [ "$c" -gt 1 ] && files="/dev/nvidia[0-$((c - 1))]"
      if [ "$mode" = check ]; then
        marker "gres=would_add node=$n files=$files"
        continue
      fi
      if [ ! -e "$gres" ]; then
        if ! (umask 022; : > "$gres") || ! chmod 0644 "$gres"; then
          marker gres=error
          echo "cannot create $gres"
          exit 11
        fi
      fi
      if [ -s "$gres" ] && [ -n "$(tail -c 1 "$gres")" ]; then
        printf '\n' >> "$gres" || { marker gres=error; exit 11; }
      fi
      if ! printf 'NodeName=%s Name=gpu File=%s\n' "$n" "$files" >> "$gres"; then
        marker gres=error
        echo "cannot write $gres"
        exit 11
      fi
      marker "gres=added node=$n files=$files"
    done <<GWKS_NODES
$listing
GWKS_NODES
  fi
  # The cloud says these have a card: Slurm gives them no gpu GRES, so a
  # gres.conf line would make slurmd refuse them; slurm.conf is theirs to fix.
  for n in $expect_gpu; do
    c=$(printf '%s\n' "$listing" | awk -v n="$n" '$1 == n { print $2; exit }')
    if [ "$c" = 0 ]; then
      marker "gres=nogres node=$n"
    fi
  done
}

resume_held_nodes() {
  listing=$(slurm_nodes)
  while read -r n c h; do
    [ "$h" = 1 ] || continue
    if [ "$mode" = check ]; then
      marker "resume=would node=$n"
    elif scontrol update nodename="$n" state=resume </dev/null; then
      marker "resume=ok node=$n"
    else
      marker "resume=failed node=$n"
    fi
  done <<GWKS_NODES
$listing
GWKS_NODES
}

case "$mode" in setup | check) add_gres_lines ;; esac

tmp=$(mktemp "${TMPDIR:-/tmp}/gwks-setup.XXXXXX") || { marker fetch=failed; exit 21; }
trap 'rm -f "$tmp"' EXIT
if ! command -v curl >/dev/null 2>&1; then
  marker "fetch=failed rc=127"
  echo "curl not found on the manager node"
  exit 21
fi
printf 'url = "%s"\n' "$setup_url" | curl -fsSL --max-time 60 -K - -o "$tmp"
fetch_rc=$?
if [ "$fetch_rc" -ne 0 ]; then
  marker "fetch=failed rc=$fetch_rc"
  exit 21
fi
case "$mode" in
  uninstall_agents) sh "$tmp" --uninstall-agents </dev/null ;;
  remove_check) sh "$tmp" --remove </dev/null ;;
  check) sh "$tmp" --check $reach_flag </dev/null ;;
  *) sh "$tmp" $reach_flag </dev/null ;;
esac
rc=$?
if [ "$rc" -eq 0 ]; then
  case "$mode" in setup | check) resume_held_nodes ;; esac
fi
marker "setup_exit=$rc"
exit "$rc"
`

// SlurmManagerScript is the script that carries out req on the manager node.
// req must have passed Validate.
func SlurmManagerScript(req SlurmApplyRequest) string {
	reachFlag := ""
	if req.SkipNodeReach {
		reachFlag = "--no-node-reach"
	}
	return strings.NewReplacer(
		"@REACHFLAG@", shellSingleQuote(reachFlag),
		"@MODE@", shellSingleQuote(req.Mode),
		"@URL@", shellSingleQuote(req.SetupURL),
		"@GRES@", shellSingleQuote(SlurmGRESConfPath),
		"@EXPECT@", shellSingleQuote(strings.Join(req.ExpectGPU, "\n")),
	).Replace(slurmManagerScript)
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// markerLine is one "GWKS_APPLY ..." or "GWKS_SLURM ..." line as key=value
// pairs.
func markerLine(line, prefix string) (map[string]string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r "), prefix+" ")
	if !ok {
		return nil, false
	}
	kv := map[string]string{}
	for _, f := range strings.Fields(rest) {
		if k, v, found := strings.Cut(f, "="); found && k != "" {
			kv[k] = v
		}
	}
	return kv, true
}

func splitNames(v string) []string {
	if v == "" || v == "none" {
		return nil
	}
	return strings.Split(v, ",")
}

// slurmSteps are the setup script's steps, by step name, as its markers say.
type slurmSteps map[string]map[string]string

// ParseSlurmApply reads what the script printed in the given mode. A run that
// stopped before finishing, or that printed no marker at all (it never ran),
// is a *SlurmApplyFailedError whose Output ends with the last lines it
// printed and, at step run, starts with ended (how the cloud says the command
// ended when it did not succeed, "" when it did). Only the GWKS_APPLY and
// GWKS_SLURM markers are read; the rest of the output is for failures.
func ParseSlurmApply(mode, output, ended string) (SlurmApplyResult, error) {
	res := SlurmApplyResult{Mode: mode}
	steps := slurmSteps{}
	var (
		shown                        []string
		sawMarker, busy, fetchFailed bool
		fetchRC                      int
		gresError                    bool
		failedStep                   string
		summarySkipped, haveSummary  bool
		summaryOK                    int
		summaryFailed                []string
		configless                   = "yes"
		comments                     []SlurmConfComment
		reached                      []SlurmReachHost
		wouldAdd                     []SlurmGresLine
		wouldResume                  []string
	)
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if kv, ok := markerLine(line, "GWKS_APPLY"); ok {
			sawMarker = true
			switch {
			case kv["busy"] != "":
				busy = true
			case kv["fetch"] == "failed":
				fetchFailed = true
				fetchRC, _ = strconv.Atoi(kv["rc"])
			case kv["gres"] == "error":
				gresError = true
			case kv["gres"] == "added":
				res.Added = append(res.Added, kv["node"])
			case kv["gres"] == "would_add":
				wouldAdd = append(wouldAdd, SlurmGresLine{Node: kv["node"], Files: kv["files"]})
			case kv["resume"] == "would":
				wouldResume = append(wouldResume, kv["node"])
			case kv["gres"] == "present":
				res.Present = append(res.Present, kv["node"])
			case kv["gres"] == "nogres":
				res.NoGPUGres = append(res.NoGPUGres, kv["node"])
			case kv["resume"] == "ok":
				res.Resumed = append(res.Resumed, kv["node"])
			case kv["resume"] == "failed":
				res.ResumeFailed = append(res.ResumeFailed, kv["node"])
			}
			continue
		}
		if kv, ok := markerLine(line, "GWKS_SLURM"); ok {
			sawMarker = true
			step := kv["step"]
			steps[step] = kv
			if kv["status"] == "failed" && failedStep == "" && step != "done" && step != "distribute" && step != "server_reach" {
				failedStep = step
			}
			switch step {
			case "conf_comment":
				n, _ := strconv.Atoi(kv["line"])
				comments = append(comments, SlurmConfComment{Line: n, Key: kv["key"], Value: kv["value"]})
			case "distribute_summary":
				haveSummary = true
				summarySkipped = kv["skipped"] == "shared"
				summaryOK, _ = strconv.Atoi(kv["ok"])
				summaryFailed = splitNames(kv["failed"])
			case "configless":
				configless = kv["value"]
			case "server_reach":
				reached = append(reached, SlurmReachHost{Role: kv["role"], Name: kv["host"], Status: kv["status"], Reason: kv["reason"]})
			}
			continue
		}
		if line = strings.TrimRight(line, "\r "); line != "" {
			shown = append(shown, line)
		}
	}
	failed := func(step, reason string) (SlurmApplyResult, error) {
		text := strings.Join(lastLines(shown, 8), "\n")
		if step == messages.SlurmApplyStepRun && ended != "" {
			text = ended + "\n" + text
		}
		return SlurmApplyResult{}, &SlurmApplyFailedError{Step: step, Reason: reason, Output: truncateTail(text, 800)}
	}
	switch {
	case busy:
		return failed(messages.SlurmApplyStepRun, messages.SlurmApplyReasonBusy)
	case gresError:
		return failed(messages.SlurmApplyStepGres, "")
	case fetchFailed:
		_, err := failed(messages.SlurmApplyStepSetup, messages.SlurmApplyReasonServerUnreachable)
		var ferr *SlurmApplyFailedError
		if errors.As(err, &ferr) {
			ferr.Kind = slurmReachReason(fetchRC)
			if ferr.Kind == "" {
				ferr.Kind = "other"
			}
		}
		return SlurmApplyResult{}, err
	case !sawMarker:
		return failed(messages.SlurmApplyStepRun, "")
	}
	if done := steps["done"]; done["status"] == "failed" || failedStep != "" {
		// The reason is the precheck's, or "<step>_failed" for the step that
		// failed (slurmboot's contract test ties each the scripts can produce
		// to its text in the UI); none when the script failed without saying
		// where, which the UI shows as unknown.
		reason := steps["precheck"]["reason"]
		if reason == "" && failedStep != "" {
			reason = failedStep + "_failed"
		}
		return failed(messages.SlurmApplyStepSetup, reason)
	}
	done := steps["done"]["status"]
	res.Done = done
	res.Reach = SlurmReach{Hosts: reached}
	switch mode {
	case SlurmModeCheck:
		if !planComplete(steps) {
			return failed(messages.SlurmApplyStepRun, "")
		}
		res.Plan = parsePlan(steps, comments, wouldAdd, wouldResume)
		return res, nil
	case SlurmModeUninstallAgents:
		if steps["uninstall_agents"]["status"] != "ok" || done == "" {
			return failed(messages.SlurmApplyStepRun, "")
		}
		res.IntervalSeconds, _ = strconv.Atoi(steps["uninstall_agents"]["interval"])
		return res, nil
	case SlurmModeRemoveCheck:
		switch steps["remove"]["status"] {
		case "ok":
		case "noop":
			res.RemoveNoop = true
		default:
			return failed(messages.SlurmApplyStepRun, "")
		}
		return res, nil
	}
	if done == "" || steps["reconfigure"]["status"] != "ok" || steps["slurm_conf"]["status"] != "ok" || steps["check_script"]["status"] != "ok" {
		return failed(messages.SlurmApplyStepRun, "")
	}
	res.Shared = steps["shared"]["status"] == "yes" || summarySkipped
	if haveSummary {
		res.DistributeOK, res.DistributeFailed = summaryOK, summaryFailed
	}
	sort.Strings(res.DistributeFailed)
	res.Steps = []SlurmApplyStep{
		{SlurmApplyIDGres, gresStatus(res)},
		{SlurmApplyIDCheckScript, proto.CheckPass},
		{SlurmApplyIDDistribute, distributeStatus(res)},
		{SlurmApplyIDSlurmConf, proto.CheckPass},
		{SlurmApplyIDReconfigure, map[bool]string{true: proto.CheckPass, false: proto.CheckWarn}[configless != "no"]},
		{SlurmApplyIDResume, resumeStatus(res)},
	}
	return res, nil
}
