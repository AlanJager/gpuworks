package provider

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// What a run on the manager node came to, as the platform keeps it, and how
// each part of it is worded.

// SlurmApplyStep is one step of a finished run: an ID above and a
// proto.Check* status (pass, warn, skip).
type SlurmApplyStep struct {
	ID     string
	Status string
}

// SlurmApplyResult is what a run on the manager node did.
type SlurmApplyResult struct {
	Mode            string
	ManagerInstance string
	// Added and Present are the nodes Slurm gives a gpu Gres by what became of
	// their gres.conf line: added now, already covered (never changed).
	Added, Present []string
	// NoGPUGres are nodes the cloud says have a card but whose Gres= lists no
	// gpu: a gres.conf line would make slurmd refuse them, so none was added;
	// slurm.conf is theirs to fix.
	NoGPUGres []string
	// Shared: the script directory is on shared storage; otherwise
	// DistributeOK nodes got the check script and DistributeFailed did not.
	Shared           bool
	DistributeOK     int
	DistributeFailed []string
	// Resumed are the nodes resumed because Slurm held them for the missing
	// GPUs, ResumeFailed those that could not be.
	Resumed, ResumeFailed []string
	// Reach is whether the server's address could be reached from the
	// controller and the compute nodes (SlurmModeSetup and SlurmModeCheck).
	Reach SlurmReach
	// Plan is what a check (SlurmModeCheck) found; nil for any other mode.
	Plan *SlurmApplyPlan
	// Done is setup's own verdict: "ok", or "partial" when something is left
	// to do by hand.
	Done string
	// IntervalSeconds is the health check interval the uninstall waits for
	// (SlurmModeUninstallAgents); RemoveNoop that --remove found nothing to
	// remove.
	IntervalSeconds int
	RemoveNoop      bool
	Steps           []SlurmApplyStep
}

func gresStatus(r SlurmApplyResult) string {
	switch {
	case len(r.NoGPUGres) > 0:
		return proto.CheckWarn
	case len(r.Added) > 0:
		return proto.CheckPass
	}
	return proto.CheckSkip
}

func distributeStatus(r SlurmApplyResult) string {
	switch {
	case r.Shared:
		return proto.CheckSkip
	case len(r.DistributeFailed) > 0:
		return proto.CheckWarn
	}
	return proto.CheckPass
}

func resumeStatus(r SlurmApplyResult) string {
	switch {
	case len(r.ResumeFailed) > 0:
		return proto.CheckWarn
	case len(r.Resumed) > 0:
		return proto.CheckPass
	}
	return proto.CheckSkip
}

// SetReach replaces what the run knows about reaching the server with the
// merged answer (mergeReach) and, for a setup, adds the step that says how it
// came out: pass when every host asked reached the server, warn when one
// could not, skip when none gave an answer; a run that asked no host has no such step.
func (r *SlurmApplyResult) SetReach(reach SlurmReach) {
	r.Reach = reach
	if r.Mode != SlurmModeSetup || len(reach.Hosts) == 0 {
		return
	}
	status := proto.CheckSkip
	switch {
	case len(reach.Failed()) > 0:
		status = proto.CheckWarn
	case reach.Asked() > 0:
		status = proto.CheckPass
	}
	r.Steps = append(r.Steps, SlurmApplyStep{SlurmApplyIDReach, status})
}

// Partial: the run finished but something is left to do by hand (a step
// that needs attention, or setup's own "partial", or a host that cannot reach
// the server).
func (r SlurmApplyResult) Partial() bool {
	if r.Done == "partial" || len(r.Reach.Failed()) > 0 {
		return true
	}
	for _, s := range r.Steps {
		if s.Status == proto.CheckWarn {
			return true
		}
	}
	return false
}

func lastLines(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

func truncateTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
