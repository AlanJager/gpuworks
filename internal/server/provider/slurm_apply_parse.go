package provider

import (
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// ParseSlurmApply reads what the script printed in the given mode. A run that
// stopped before finishing, or that printed no marker at all (it never ran),
// is a *SlurmApplyFailedError whose Output ends with the last lines it
// printed and, at step run, starts with ended (how the cloud says the command
// ended when it did not succeed, "" when it did). Only the GWKS_APPLY and
// GWKS_SLURM markers are read; the rest of the output is for failures.
func ParseSlurmApply(mode, output, ended string) (SlurmApplyResult, error) {
	m := applyMarkers{res: SlurmApplyResult{Mode: mode}, steps: slurmSteps{}, configless: "yes", ended: ended}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if kv, ok := markerLine(line, "GWKS_APPLY"); ok {
			m.apply(kv)
			continue
		}
		if kv, ok := markerLine(line, "GWKS_SLURM"); ok {
			m.slurm(kv)
			continue
		}
		if line = strings.TrimRight(line, "\r "); line != "" {
			m.shown = append(m.shown, line)
		}
	}
	if err := m.failure(); err != nil {
		return SlurmApplyResult{}, err
	}
	return m.result(mode)
}

// applyMarkers is what a run printed, as ParseSlurmApply reads it.
type applyMarkers struct {
	res   SlurmApplyResult
	steps slurmSteps
	ended string
	// shown are the lines that were not markers, for a failure's output.
	shown []string

	sawMarker, busy, fetchFailed, gresError bool
	fetchRC                                 int
	// failedStep is the first step that marked itself failed (not done,
	// distribute or server_reach, which are not failures of the run).
	failedStep                  string
	summarySkipped, haveSummary bool
	summaryOK                   int
	summaryFailed               []string
	configless                  string
	comments                    []SlurmConfComment
	reached                     []SlurmReachHost
	wouldAdd                    []SlurmGresLine
	wouldResume                 []string
}

// apply reads a GWKS_APPLY marker, printed by the manager script around setup.
func (m *applyMarkers) apply(kv map[string]string) {
	m.sawMarker = true
	switch {
	case kv["busy"] != "":
		m.busy = true
	case kv["fetch"] == "failed":
		m.fetchFailed = true
		m.fetchRC, _ = strconv.Atoi(kv["rc"])
	case kv["gres"] == "error":
		m.gresError = true
	case kv["gres"] == "added":
		m.res.Added = append(m.res.Added, kv["node"])
	case kv["gres"] == "would_add":
		m.wouldAdd = append(m.wouldAdd, SlurmGresLine{Node: kv["node"], Files: kv["files"]})
	case kv["resume"] == "would":
		m.wouldResume = append(m.wouldResume, kv["node"])
	case kv["gres"] == "present":
		m.res.Present = append(m.res.Present, kv["node"])
	case kv["gres"] == "nogres":
		m.res.NoGPUGres = append(m.res.NoGPUGres, kv["node"])
	case kv["resume"] == "ok":
		m.res.Resumed = append(m.res.Resumed, kv["node"])
	case kv["resume"] == "failed":
		m.res.ResumeFailed = append(m.res.ResumeFailed, kv["node"])
	}
}

// slurm reads a GWKS_SLURM step marker, printed by setup.sh.
func (m *applyMarkers) slurm(kv map[string]string) {
	m.sawMarker = true
	step := kv["step"]
	m.steps[step] = kv
	if kv["status"] == "failed" && m.failedStep == "" && step != "done" && step != "distribute" && step != "server_reach" {
		m.failedStep = step
	}
	switch step {
	case "conf_comment":
		n, _ := strconv.Atoi(kv["line"])
		m.comments = append(m.comments, SlurmConfComment{Line: n, Key: kv["key"], Value: kv["value"]})
	case "distribute_summary":
		m.haveSummary = true
		m.summarySkipped = kv["skipped"] == "shared"
		m.summaryOK, _ = strconv.Atoi(kv["ok"])
		m.summaryFailed = splitNames(kv["failed"])
	case "configless":
		m.configless = kv["value"]
	case "server_reach":
		m.reached = append(m.reached, SlurmReachHost{Role: kv["role"], Name: kv["host"], Status: kv["status"], Reason: kv["reason"]})
	}
}

// failed is the error of a run that failed at step, for reason.
func (m *applyMarkers) failed(step, reason string) *SlurmApplyFailedError {
	text := strings.Join(lastLines(m.shown, 8), "\n")
	if step == messages.SlurmApplyStepRun && m.ended != "" {
		text = m.ended + "\n" + text
	}
	return &SlurmApplyFailedError{Step: step, Reason: reason, Output: truncateTail(text, 800)}
}

// failure is why the run failed whatever the mode, nil when it did not.
func (m *applyMarkers) failure() error {
	switch {
	case m.busy:
		return m.failed(messages.SlurmApplyStepRun, messages.SlurmApplyReasonBusy)
	case m.gresError:
		return m.failed(messages.SlurmApplyStepGres, "")
	case m.fetchFailed:
		err := m.failed(messages.SlurmApplyStepSetup, messages.SlurmApplyReasonServerUnreachable)
		err.Kind = slurmReachReason(m.fetchRC)
		if err.Kind == "" {
			err.Kind = "other"
		}
		return err
	case !m.sawMarker:
		return m.failed(messages.SlurmApplyStepRun, "")
	}
	if m.steps["done"]["status"] == "failed" || m.failedStep != "" {
		// The reason is the precheck's, or "<step>_failed" for the step that
		// failed (slurmboot's contract test ties each the scripts can produce
		// to its text in the UI); none when the script failed without saying
		// where, which the UI shows as unknown.
		reason := m.steps["precheck"]["reason"]
		if reason == "" && m.failedStep != "" {
			reason = m.failedStep + "_failed"
		}
		return m.failed(messages.SlurmApplyStepSetup, reason)
	}
	return nil
}

// result is what a run that did not fail did, as mode reads it; a run that
// did not get to the markers its mode ends with failed at step run.
func (m *applyMarkers) result(mode string) (SlurmApplyResult, error) {
	res, steps := m.res, m.steps
	done := steps["done"]["status"]
	res.Done = done
	res.Reach = SlurmReach{Hosts: m.reached}
	incomplete := func() (SlurmApplyResult, error) {
		return SlurmApplyResult{}, m.failed(messages.SlurmApplyStepRun, "")
	}
	switch mode {
	case SlurmModeCheck:
		if !planComplete(steps) {
			return incomplete()
		}
		res.Plan = parsePlan(steps, m.comments, m.wouldAdd, m.wouldResume)
		return res, nil
	case SlurmModeUninstallAgents:
		if steps["uninstall_agents"]["status"] != "ok" || done == "" {
			return incomplete()
		}
		res.IntervalSeconds, _ = strconv.Atoi(steps["uninstall_agents"]["interval"])
		return res, nil
	case SlurmModeRemoveCheck:
		switch steps["remove"]["status"] {
		case "ok":
		case "noop":
			res.RemoveNoop = true
		default:
			return incomplete()
		}
		return res, nil
	}
	if done == "" || steps["reconfigure"]["status"] != "ok" || steps["slurm_conf"]["status"] != "ok" || steps["check_script"]["status"] != "ok" {
		return incomplete()
	}
	res.Shared = steps["shared"]["status"] == "yes" || m.summarySkipped
	if m.haveSummary {
		res.DistributeOK, res.DistributeFailed = m.summaryOK, m.summaryFailed
	}
	sort.Strings(res.DistributeFailed)
	res.Steps = []SlurmApplyStep{
		{SlurmApplyIDGres, gresStatus(res)},
		{SlurmApplyIDCheckScript, proto.CheckPass},
		{SlurmApplyIDDistribute, distributeStatus(res)},
		{SlurmApplyIDSlurmConf, proto.CheckPass},
		{SlurmApplyIDReconfigure, map[bool]string{true: proto.CheckPass, false: proto.CheckWarn}[m.configless != "no"]},
		{SlurmApplyIDResume, resumeStatus(res)},
	}
	return res, nil
}
