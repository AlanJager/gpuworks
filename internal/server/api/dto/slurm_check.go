package dto

// SlurmApplyRequest is the body of POST /slurm/clusters/{uuid}/apply:
// DryRun is required, so applying is never taken by omission. True runs the
// check alone, which changes nothing on the cluster and answers the plan
// (SlurmApplyCheck); false carries the setup out (SlurmApplyOutcome).
type SlurmApplyRequest struct {
	DryRun *bool `json:"dry_run"`
}

// SlurmApplyCheck is what a setup would do on the cluster, the answer to a
// dry run: the facts the setup acts on, read now, with nothing changed.
// Status is "ok" or "partial" (something will be left to do by hand, see
// Warnings), and every field below is the server's reading: a client shows it.
type SlurmApplyCheck struct {
	DryRun          bool   `json:"dry_run"`
	Status          string `json:"status"`
	ManagerInstance string `json:"manager_instance"`
	// SlurmCluster is the cluster's own ClusterName.
	SlurmCluster string           `json:"slurm_cluster"`
	Gres         SlurmCheckGres   `json:"gres"`
	Script       SlurmCheckScript `json:"script"`
	Conf         SlurmCheckConf   `json:"conf"`
	// Configless: slurmctld serves its configuration to the nodes, so they
	// pick the change up on the reconfigure; otherwise slurm.conf has to be
	// copied to them by hand.
	Configless bool `json:"configless"`
	// Rearm: an earlier uninstall of the agents is armed and would be undone.
	Rearm bool `json:"rearm"`
	// Resume are the nodes Slurm holds for the missing GPUs that would be
	// resumed.
	Resume []string `json:"resume_nodes"`
	// Warnings are codes: conf_include, program_not_in_conf, not_configless,
	// no_gpu_gres, server_unreachable; each makes the setup finish "partial".
	Warnings []string `json:"warnings"`
	// Reach is whether the server's address can be reached from the
	// controller and from each GPU compute node.
	Reach   SlurmReach   `json:"reach"`
	Cluster SlurmCluster `json:"cluster"`
}

// SlurmCheckGres is what would become of gres.conf, by node name.
type SlurmCheckGres struct {
	// WouldAdd are the lines it would append, for GPU nodes Slurm knows that
	// no gpu line covers, with the cards Slurm gives each; Present are the
	// nodes already covered (never changed) and NoGPUGres the nodes the cloud
	// account lists with a card that Slurm gives no gpu GRES (nothing is added
	// for them).
	WouldAdd  []SlurmCheckGresLine `json:"would_add"`
	Present   []string             `json:"present"`
	NoGPUGres []string             `json:"no_gpu_gres"`
}

// SlurmCheckGresLine is a gres.conf line the setup would append: Line is its
// text, "NodeName=<node> Name=gpu File=<files>".
type SlurmCheckGresLine struct {
	Node  string `json:"node"`
	Files string `json:"files"`
	Line  string `json:"line"`
}

// SlurmCheckScript is where the health check program goes and how it reaches
// the compute nodes.
type SlurmCheckScript struct {
	CheckScript string `json:"check_script"`
	TokenFile   string `json:"token_file"`
	// Shared: the script directory is on shared storage (FSType), so nothing
	// is copied; otherwise CopyTo are the nodes it copies CopyFiles to.
	Shared    bool     `json:"shared"`
	FSType    string   `json:"fstype"`
	CopyTo    []string `json:"copy_to"`
	CopyFiles []string `json:"copy_files"`
}

// SlurmCheckConf is the edit of slurm.conf: the block it would add, the
// cluster's own HealthCheck settings it would keep as comments and chain.
type SlurmCheckConf struct {
	Path string `json:"path"`
	// Changed: the edit would change the file; false when it is already as
	// the setup would write it.
	Changed   bool   `json:"changed"`
	Interval  int    `json:"interval"`
	NodeState string `json:"node_state"`
	// Block are the settings of the block it would add, as written.
	Block []string `json:"block"`
	// ChainedProgram is the cluster's own health check program that would run
	// after ours, in ChainedState; both empty without one.
	ChainedProgram string `json:"chained_program,omitempty"`
	ChainedState   string `json:"chained_state,omitempty"`
	// Commented are the settings it would turn into comments in place.
	Commented []SlurmCheckComment `json:"commented"`
	// Includes are the files slurm.conf includes, whose settings the edit does
	// not see; UnnamedProgram the health check program slurmctld runs that
	// slurm.conf does not name.
	Includes       []string `json:"includes"`
	UnnamedProgram string   `json:"unnamed_program,omitempty"`
}

// SlurmCheckComment is a HealthCheck setting of slurm.conf the setup would
// comment: its line, key and value.
type SlurmCheckComment struct {
	Line  int    `json:"line"`
	Key   string `json:"key"`
	Value string `json:"value"`
}
