package api

import (
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/provider"
)

// slurmApplyCheck is what a check-only run found, as the API shows it.
func slurmApplyCheck(res provider.SlurmApplyResult, cluster dto.SlurmCluster) dto.SlurmApplyCheck {
	plan := res.Plan
	out := dto.SlurmApplyCheck{
		DryRun: true, Status: "ok", ManagerInstance: res.ManagerInstance, SlurmCluster: plan.SlurmCluster,
		Gres: dto.SlurmCheckGres{WouldAdd: make([]dto.SlurmCheckGresLine, 0, len(plan.GresAdd)),
			Present: nonNil(res.Present), NoGPUGres: nonNil(res.NoGPUGres)},
		Script: dto.SlurmCheckScript{CheckScript: plan.CheckScript, TokenFile: plan.TokenFile, Shared: plan.Shared, FSType: plan.FSType,
			CopyTo: nonNil(plan.CopyTo), CopyFiles: nonNil(plan.CopyFiles)},
		Conf: dto.SlurmCheckConf{Path: plan.ConfPath, Changed: plan.ConfChanged, Interval: plan.Interval, NodeState: plan.NodeState,
			Block:          []string{"HealthCheckProgram=" + plan.CheckScript, "HealthCheckInterval=" + strconv.Itoa(plan.Interval), "HealthCheckNodeState=" + plan.NodeState},
			ChainedProgram: plan.ChainedProgram, ChainedState: plan.ChainedState, Commented: make([]dto.SlurmCheckComment, 0, len(plan.Commented)),
			Includes: nonNil(plan.Includes), UnnamedProgram: plan.UnnamedProgram},
		Configless: plan.Configless, Rearm: plan.Rearm, Resume: nonNil(plan.Resume), Warnings: nonNil(res.PlanWarnings()), Reach: slurmReachDTO(res.Reach), Cluster: cluster,
	}
	for _, l := range plan.GresAdd {
		out.Gres.WouldAdd = append(out.Gres.WouldAdd, dto.SlurmCheckGresLine{Node: l.Node, Files: l.Files, Line: provider.SlurmGresLineText(l)})
	}
	for _, c := range plan.Commented {
		out.Conf.Commented = append(out.Conf.Commented, dto.SlurmCheckComment{Line: c.Line, Key: c.Key, Value: c.Value})
	}
	if res.Partial() || len(out.Warnings) > 0 {
		out.Status = "partial"
	}
	return out
}
