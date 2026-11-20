package app

import (
	"context"
	"os/exec"

	"gpuworks.ai/gpuworks/internal/agent/slurm"
)

// startReporting has an enrolled agent report its cluster's GPU nodes to the
// server, and take the entry's settings from each answer: the leaser applies
// them to the jobs it submits from then on, and the next leaser (the node is
// asked about again until Slurm answers) starts with them. A host without
// sinfo has nothing to report.
func (s *slurmNode) startReporting(ctx context.Context) {
	e := s.enrolled
	if e == nil || e.Server == "" || e.Token == nil {
		return
	}
	if _, err := exec.LookPath("sinfo"); err != nil {
		return
	}
	r := &slurm.Reporter{Server: e.Server, Token: e.Token, Command: slurm.ExecRunner, Log: s.log,
		Apply: func(settings slurm.Settings) {
			s.latest.Store(&settings)
			if l := s.holders.Load(); l != nil {
				l.SetSettings(settings)
			}
		}}
	go r.Run(ctx)
}
