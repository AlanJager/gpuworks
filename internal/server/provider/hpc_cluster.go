package provider

import "errors"

// HPCCluster is the vendor's HPC cluster an instance is a compute node of.
// Like a managed Kubernetes cluster, its nodes join through the cluster, as a
// Slurm cluster entry whose health check installs the agents (the agent finds
// the node's scheduler and borrows cards from it,
// docs/adr/0075-slurm-allocates-node-cards.md); none is installed on one node
// by itself. What the cluster owns is the node's lifecycle: its queue adds and
// releases nodes, so the platform does not power, release or resize them.
type HPCCluster struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Scheduler is the cluster's workload manager, lower case ("slurm",
	// "pbs"…); SchedulerVersion its version as the vendor reports it.
	Scheduler        string `json:"scheduler,omitempty"`
	SchedulerVersion string `json:"scheduler_version,omitempty"`
}

// SchedulerSlurm is the only scheduler whose nodes an agent shares cards
// with; on a node of another scheduler, the agent and the scheduler's jobs
// would both take the same cards.
const SchedulerSlurm = "slurm"

// HPCConnectable reports whether an HPC cluster under scheduler can be
// connected as a Slurm cluster entry: only Slurm's cards the agent shares.
func HPCConnectable(scheduler string) bool {
	return scheduler == SchedulerSlurm
}

// ErrInstanceInHPCCluster: the instance is a compute node of an HPC cluster,
// whose queue owns its lifecycle.
var ErrInstanceInHPCCluster = errors.New("the instance is a compute node of an HPC cluster")

// InstanceInHPCClusterError is ErrInstanceInHPCCluster for one cluster.
type InstanceInHPCClusterError struct{ Cluster, Scheduler string }

func (e *InstanceInHPCClusterError) Error() string {
	return ErrInstanceInHPCCluster.Error() + " (" + e.Cluster + "): add and release its nodes through the cluster's queue"
}

func (e *InstanceInHPCClusterError) Unwrap() error { return ErrInstanceInHPCCluster }

// refuseHPCNode refuses powering, releasing or resizing an instance the last
// sync listed as a compute node of an HPC cluster: behind the queue's back,
// the scheduler marks the node down and the queue may replace it.
func (s *Service) refuseHPCNode(providerID uint, externalID string) error {
	row, err := s.store.ProviderInstanceByExternalID(providerID, externalID)
	if err != nil {
		return err
	}
	if row != nil && row.HPCClusterID != "" {
		return &InstanceInHPCClusterError{Cluster: row.HPCClusterID, Scheduler: row.HPCScheduler}
	}
	return nil
}
