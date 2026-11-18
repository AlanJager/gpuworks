package messages

// Slurm cluster entries (/slurm/clusters): why a request about one is refused.
// The enrollment refusal (SLURM_ENROLL_REFUSED) is a code of the agent's
// protocol and lives in package proto.
const (
	// ErrorSlurmClusterInvalid: a cluster parameter cannot be used. Params
	// field is the request field and reason why.
	ErrorSlurmClusterInvalid = "SLURM_CLUSTER_INVALID"
	// ErrorSlurmClusterNameTaken: another Slurm cluster entry already has the
	// name. Param name.
	ErrorSlurmClusterNameTaken = "SLURM_CLUSTER_NAME_TAKEN"
	// ErrorSlurmClusterHasNodes: the cluster still has nodes; remove them
	// first.
	ErrorSlurmClusterHasNodes = "SLURM_CLUSTER_HAS_NODES"
	// ErrorSlurmClusterNodesOnline: deleting the cluster with its nodes was
	// asked for, but some of their agents are connected: uninstall them first
	// (the agents go offline once they have uninstalled). Param count.
	ErrorSlurmClusterNodesOnline = "SLURM_CLUSTER_NODES_ONLINE"
	// ErrorSlurmClusterNodesHaveInstances: deleting the cluster with its nodes
	// was asked for, but some of the nodes have instances: remove those first.
	// Param count is the nodes that do.
	ErrorSlurmClusterNodesHaveInstances = "SLURM_CLUSTER_NODES_HAVE_INSTANCES"
	// ErrorSlurmClusterSourceTaken: an entry was already made from the cloud
	// account's cluster. Param cluster.
	ErrorSlurmClusterSourceTaken = "SLURM_CLUSTER_SOURCE_TAKEN"
)
