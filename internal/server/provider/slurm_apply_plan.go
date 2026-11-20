package provider

import (
	"strconv"
	"strings"
)

// What a check-only run on the manager node (SlurmModeCheck) found: the plan
// of a setup, from the same facts the setup acts on and with nothing changed.

// SlurmGresLine is a gres.conf line setup would append for a node.
type SlurmGresLine struct {
	Node  string
	Files string
}

// SlurmGresLineText is the line as it is written to gres.conf.
func SlurmGresLineText(l SlurmGresLine) string {
	return "NodeName=" + l.Node + " Name=gpu File=" + l.Files
}

// SlurmConfComment is a HealthCheck setting of slurm.conf (the line it is on,
// its key as slurm.conf spells it in the plan, its value) that setup would
// keep as a comment, the cluster's own program being chained instead.
type SlurmConfComment struct {
	Line       int
	Key, Value string
}

// SlurmApplyPlan is what a setup would do on the cluster.
type SlurmApplyPlan struct {
	// SlurmCluster is the cluster's own ClusterName.
	SlurmCluster string
	// GresAdd are the gres.conf lines it would append; the nodes already
	// covered or given no gpu GRES are SlurmApplyResult's Present and
	// NoGPUGres, which a check fills like a setup does.
	GresAdd []SlurmGresLine
	// CheckScript and TokenFile are the paths it would write in the script
	// directory.
	CheckScript, TokenFile string
	// Shared: the script directory is on shared storage (FSType); otherwise
	// CopyTo are the nodes the files would be copied to, CopyFiles the files.
	Shared    bool
	FSType    string
	CopyTo    []string
	CopyFiles []string
	// ConfPath is the slurm.conf it would edit; ConfChanged says the edit
	// would change it (a second run finds it already as written).
	ConfPath    string
	ConfChanged bool
	// Interval and NodeState are the HealthCheck settings of the block it
	// would add; ChainedProgram and ChainedState the cluster's own program it
	// would chain and the node states that program keeps running in, empty
	// when there is none. Commented are the settings it would comment.
	Interval       int
	NodeState      string
	ChainedProgram string
	ChainedState   string
	Commented      []SlurmConfComment
	// Includes are the files slurm.conf includes (settings in them are not
	// seen), UnnamedProgram the health check program slurmctld runs that
	// slurm.conf does not name.
	Includes       []string
	UnnamedProgram string
	// Configless: slurmctld serves its configuration to the nodes.
	Configless bool
	// Rearm: an earlier uninstall of the agents is armed and would be undone.
	Rearm bool
	// Resume are the nodes held for the missing GPUs that would be resumed.
	Resume []string
}

// parsePlan reads a check run's plan from its markers. configless, comments
// and wouldAdd were collected as the lines went by.
func parsePlan(steps slurmSteps, comments []SlurmConfComment, wouldAdd []SlurmGresLine, resume []string) *SlurmApplyPlan {
	plan := &SlurmApplyPlan{
		SlurmCluster: steps["precheck"]["cluster"],
		GresAdd:      wouldAdd,
		CheckScript:  steps["check_script"]["path"],
		TokenFile:    steps["token_file"]["path"],
		Shared:       steps["shared"]["status"] == "yes",
		FSType:       steps["shared"]["fstype"],
		ConfPath:     steps["slurm_conf"]["path"],
		ConfChanged:  steps["slurm_conf"]["changed"] == "yes",
		NodeState:    steps["slurm_conf"]["node_state"],
		Configless:   steps["configless"]["value"] != "no",
		Rearm:        steps["install_mode"]["rearm"] == "yes",
		Commented:    comments,
		Resume:       resume,
	}
	plan.Interval, _ = strconv.Atoi(steps["slurm_conf"]["interval"])
	if program := steps["chained"]["program"]; program != "none" {
		plan.ChainedProgram, plan.ChainedState = program, steps["chained"]["node_state"]
	}
	if d := steps["distribute_plan"]; d["skipped"] == "" {
		plan.CopyTo = splitNames(d["nodes"])
	}
	plan.CopyFiles = strings.Split(steps["distribute_plan"]["files"], ",")
	if files := steps["conf_include"]["files"]; files != "" {
		plan.Includes = strings.Split(files, ",")
	}
	if steps["conf_source"]["reason"] == "program_not_in_conf" {
		plan.UnnamedProgram = steps["conf_source"]["program"]
	}
	return plan
}

// planComplete: a check run printed every step of the plan; one that did not
// stopped before finishing.
func planComplete(steps slurmSteps) bool {
	for _, s := range []string{"precheck", "check_script", "token_file", "chained", "shared", "distribute_plan", "slurm_conf", "configless", "reconfigure"} {
		if steps[s] == nil {
			return false
		}
	}
	return steps["done"]["mode"] == "check" && steps["slurm_conf"]["status"] == "plan"
}

// The warnings a plan carries, by code: each names a thing the operator is told
// before confirming, and each is also what makes the setup finish partial.
const (
	SlurmPlanWarnInclude        = "conf_include"
	SlurmPlanWarnProgramUnnamed = "program_not_in_conf"
	SlurmPlanWarnNotConfigless  = "not_configless"
	SlurmPlanWarnNoGPUGres      = "no_gpu_gres"
	SlurmPlanWarnServerUnreach  = "server_unreachable"
)

// PlanWarnings are the codes (SlurmPlanWarn*) of what the plan says needs
// attention, in a stable order; the run is then partial. Nil without a plan.
func (r SlurmApplyResult) PlanWarnings() []string {
	if r.Plan == nil {
		return nil
	}
	var out []string
	if len(r.Plan.Includes) > 0 {
		out = append(out, SlurmPlanWarnInclude)
	}
	if r.Plan.UnnamedProgram != "" {
		out = append(out, SlurmPlanWarnProgramUnnamed)
	}
	if !r.Plan.Configless {
		out = append(out, SlurmPlanWarnNotConfigless)
	}
	if len(r.NoGPUGres) > 0 {
		out = append(out, SlurmPlanWarnNoGPUGres)
	}
	if len(r.Reach.Failed()) > 0 {
		out = append(out, SlurmPlanWarnServerUnreach)
	}
	return out
}
