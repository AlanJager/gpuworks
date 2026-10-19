package dto

import "time"

// KubeCluster is a Kubernetes cluster whose GPU nodes join through one
// DaemonSet. ApplyCommand carries the enrollment token and is present for
// administrators only.
type KubeCluster struct {
	UUID              string    `json:"uuid"`
	Name              string    `json:"name"`
	Namespace         string    `json:"namespace"`
	ServerURL         string    `json:"server_url"`
	AgentImage        string    `json:"agent_image"`
	NodeSelector      string    `json:"node_selector"`
	RuntimeClass      string    `json:"runtime_class"`
	ContainerdSocket  string    `json:"containerd_socket"`
	EnginePorts       string    `json:"engine_ports"`
	IdleReturnSeconds int       `json:"idle_return_seconds"` // -1 keeps borrowed cards
	ClusterUID        string    `json:"cluster_uid"`         // empty until the first node enrolls
	Revoked           bool      `json:"revoked"`
	NodeCount         int       `json:"node_count"`
	ApplyCommand      string    `json:"apply_command,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// KubeClusterRequest creates a cluster. Empty optional fields take the
// defaults the response shows.
type KubeClusterRequest struct {
	Name              string  `json:"name"`
	ServerURL         string  `json:"server_url"`
	Namespace         string  `json:"namespace,omitempty"`
	AgentImage        string  `json:"agent_image,omitempty"`
	NodeSelector      string  `json:"node_selector,omitempty"`
	RuntimeClass      *string `json:"runtime_class,omitempty"`
	ContainerdSocket  string  `json:"containerd_socket,omitempty"`
	EnginePorts       string  `json:"engine_ports,omitempty"`
	IdleReturnSeconds *int    `json:"idle_return_seconds,omitempty"`
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
