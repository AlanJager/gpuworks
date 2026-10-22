package dto

import "time"

// KubeCluster is a Kubernetes cluster whose GPU nodes join through one
// DaemonSet. ApplyCommand carries the enrollment token and is present for
// administrators only.
type KubeCluster struct {
	ID                uint   `json:"id"` // what NodeKube.ClusterID refers to
	UUID              string `json:"uuid"`
	Name              string `json:"name"`
	Namespace         string `json:"namespace"`
	ServerURL         string `json:"server_url"`
	AgentImage        string `json:"agent_image"`
	Vendor            string `json:"vendor"`
	NodeSelector      string `json:"node_selector"`
	RuntimeClass      string `json:"runtime_class"`     // empty: the cluster's "nvidia" class when it has one, else the default runtime
	ImagePullSecret   string `json:"image_pull_secret"` // empty: the agent image is pulled without credentials
	ContainerdSocket  string `json:"containerd_socket"` // empty: the agents find it
	StateDir          string `json:"state_dir"`
	APIServer         string `json:"api_server"` // empty: the agents use the cluster's "kubernetes" Service
	EnginePorts       string `json:"engine_ports"`
	IdleReturnSeconds int    `json:"idle_return_seconds"` // -1 keeps borrowed cards
	ClusterUID        string `json:"cluster_uid"`         // empty until the first node enrolls
	Revoked           bool   `json:"revoked"`
	NodeCount         int    `json:"node_count"`
	ApplyCommand      string `json:"apply_command,omitempty"`
	// Source is the cloud account cluster the entry was made from; the
	// server can then apply the manifest itself (POST …/apply).
	Source    *KubeClusterSource `json:"source,omitempty"`
	CreatedAt time.Time          `json:"created_at"`
}

// KubeClusterRequest creates a cluster. Empty optional fields take the
// defaults the response shows.
type KubeClusterRequest struct {
	Name              string `json:"name"`
	ServerURL         string `json:"server_url"`
	Namespace         string `json:"namespace,omitempty"`
	AgentImage        string `json:"agent_image,omitempty"`
	Vendor            string `json:"vendor,omitempty"`
	NodeSelector      string `json:"node_selector,omitempty"`
	RuntimeClass      string `json:"runtime_class,omitempty"`
	ImagePullSecret   string `json:"image_pull_secret,omitempty"`
	ContainerdSocket  string `json:"containerd_socket,omitempty"`
	StateDir          string `json:"state_dir,omitempty"`
	APIServer         string `json:"api_server,omitempty"`
	EnginePorts       string `json:"engine_ports,omitempty"`
	IdleReturnSeconds *int   `json:"idle_return_seconds,omitempty"`
	// ProviderID and ProviderClusterID name the cloud account cluster the
	// entry is made from (GET /providers/{id}/clusters); both or neither.
	ProviderID        *uint  `json:"provider_id,omitempty"`
	ProviderClusterID string `json:"provider_cluster_id,omitempty"`
}

// KubeEnrollRequest is what an agent of a cluster's DaemonSet sends to trade
// the enrollment token for its node token.
type KubeEnrollRequest struct {
	Token      string `json:"token"`
	ClusterUID string `json:"cluster_uid"`
	NodeName   string `json:"node_name"`
}

// KubeEnrollResponse is the node the agent now is.
type KubeEnrollResponse struct {
	NodeUUID          string `json:"node_uuid"`
	NodeToken         string `json:"node_token"`
	IdleReturnSeconds int    `json:"idle_return_seconds"`
}

// KubeClusterSource is the cloud account cluster an entry was made from.
type KubeClusterSource struct {
	ProviderID   uint   `json:"provider_id"`
	ProviderName string `json:"provider_name"` // empty when the account is gone from the listing
	ProviderKind string `json:"provider_kind"`
	ClusterID    string `json:"cluster_id"`
}

// ProviderCluster is a managed Kubernetes cluster in a cloud account
// (provider.Cluster) and the entry made from it, if any.
type ProviderCluster struct {
	ExternalID string                `json:"external_id"`
	Name       string                `json:"name"`
	Region     string                `json:"region"`
	Version    string                `json:"version"`
	State      string                `json:"state"`
	Running    bool                  `json:"running"`
	PublicAPI  bool                  `json:"public_api"`
	GPUPools   []ProviderClusterPool `json:"gpu_pools"`
	Entry      *KubeClusterRef       `json:"entry,omitempty"`
}

// ProviderClusterPool is one GPU node pool of a ProviderCluster.
type ProviderClusterPool struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	GPUModel     string `json:"gpu_model"`
	GPUCount     int    `json:"gpu_count"`
	Nodes        int    `json:"nodes"`
	Vendor       string `json:"vendor"`
	NodeSelector string `json:"node_selector"`
}

// KubeClusterRef names a cluster entry.
type KubeClusterRef struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

// KubeClusterOperationRequest asks for an apply or a removal.
type KubeClusterOperationRequest struct {
	DryRun bool `json:"dry_run"`
}

// KubeClusterOperation is what an apply or a removal did to each object of
// the manifest. A failed operation carries the code, error and params of a
// product error beside the results it got to.
type KubeClusterOperation struct {
	DryRun  bool                      `json:"dry_run"`
	Results []KubeObjectResult        `json:"results"`
	Code    string                    `json:"code,omitempty"`
	Error   string                    `json:"error,omitempty"`
	Params  *KubeOperationErrorParams `json:"params,omitempty"`
}

// KubeOperationErrorParams says where an operation failed: "kubeconfig"
// (the cloud would not issue one, or it cannot be used here) or
// "api_server" (the cluster refused).
type KubeOperationErrorParams struct {
	Step   string `json:"step"`
	Reason string `json:"reason"`
}

// KubeObjectResult is one object of an operation (kubeapply.Result).
type KubeObjectResult struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Action: create | update | delete | absent | kept | failed | not_tried | dependent.
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}
