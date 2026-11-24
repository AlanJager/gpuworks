// Package plane is what the inference gateway needs from the control plane,
// as interfaces and plain data: the routing rows it serves from, the lookups a
// request still makes, the reports it sends back, and the few calls that need
// the control plane itself. The gateway holds these interfaces only, never a
// store handle, the agent hub or the event bus, so that what it serves from
// can later be supplied by another member, or across a process boundary.
//
// The row types are still the store's entities: planning and deploy, which the
// gateway calls with them, take those types. They are data here, not a handle.
package plane

import (
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tunnelerr"
)

// Topics and reasons of a Change the gateway tells apart. They equal the
// store's and the bus's own strings (storeplane's test holds them equal).
const (
	TopicDeployment        = "deployment"
	TopicInstance          = "instance"
	TopicNode              = "node"
	TopicNetwork           = "network"
	TopicModel             = "model"
	TopicInferenceTemplate = "inference_template"

	// ReasonProgress marks a change that only moved progress counters.
	ReasonProgress = "progress"
	// ReasonNodeStatus is a node's routine status report.
	ReasonNodeStatus = "status"
	// ReasonNodeScheduling is a change of a node's scheduling state.
	ReasonNodeScheduling = "scheduling"
)

// ErrNotFound is what a lookup returns for a row that does not exist.
var ErrNotFound = store.ErrNotFound

// ErrDataLaneUnavailable is what opening a tunnel returns when the node's
// data lane was detached and did not reattach within the wait the tunnel
// provider allows.
var ErrDataLaneUnavailable = tunnelerr.ErrDataLaneUnavailable

// Change is one announced change. It carries no payload: the gateway reads
// the current state again.
type Change struct {
	Topic  string `json:"topic"`
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// Snapshot is the rows the gateway builds its route table from, read at one
// moment. It is plain data and survives encoding unchanged.
type Snapshot struct {
	// Generation is one more than the count of instance and deployment
	// changes announced before the rows were read: a candidate built from
	// the rows is exactly what a read returns while that count stands.
	Generation  uint64             `json:"generation"`
	Deployments []store.Deployment `json:"deployments"`
	Instances   []store.Instance   `json:"instances"`
	// NodeUUID and NodeDriver are each node's UUID and runtime driver, by
	// node id.
	NodeUUID   map[uint]string `json:"node_uuid"`
	NodeDriver map[uint]string `json:"node_driver"`
}

// Snapshots supplies the routing rows and says when they changed.
type Snapshots interface {
	// Snapshot reads the routing rows now.
	Snapshot() (*Snapshot, error)
	// Changes delivers the changes of the given topics until stop is called.
	Changes(buffer int, topics ...string) (changes <-chan Change, stop func())
	// Generation counts the changes announced on a topic.
	Generation(topic string) uint64
	// Announces reports whether changes are announced at all; without it
	// Generation stands at zero and proves nothing.
	Announces() bool
}

// Lookups are the reads a request makes outside the route table: the key it
// presents, and the rows of a name the table could not route.
type Lookups interface {
	ResolveAPIKey(presented string) (store.APIKeyIdentity, bool)
	APIKeyScopeByID(id uint) (store.APIKeyIdentity, error)
	InferenceOpenAccess() (bool, error)
	PriorityClassGeneration() uint64
	PriorityClassTable() (priority.Table, error)

	DeploymentsByServedName(name string) ([]store.Deployment, error)
	GetDeployment(id uint) (*store.Deployment, error)
	InstancesForDeployment(deploymentID uint) ([]store.Instance, error)
	InstanceByUUID(uuid string) (*store.Instance, error)
	NodeByID(id uint) (*store.Node, error)
	GetModel(id uint) (*store.Model, error)
	TemplateSpecAPIContracts(keys []store.TemplateSpecKey) (map[store.TemplateSpecKey]string, error)
}

// Reports are what the gateway writes back while it serves.
type Reports interface {
	// SetNodeServingForward records how the gateway last reached a node
	// over its serving network.
	SetNodeServingForward(nodeUUID string, status store.ServingForwardStatus) error
}

// ControlPlane are the calls that need the control plane itself and have no
// place in a snapshot: the Playground's contract of a deployment's model and
// its external model configurations with their keys.
type ControlPlane interface {
	DeploymentContract(deployment *store.Deployment) inferenceapi.Contract
	ListAIConfigs() ([]store.AIConfig, error)
	DecryptAIKey(box *auth.Secretbox, sealed string) (string, error)
}

// Source is everything the gateway is given.
type Source interface {
	Snapshots
	Lookups
	Reports
	ControlPlane
}
