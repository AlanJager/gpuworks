package dto

import (
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type InstanceLaunchFacts struct {
	Intent                       LaunchIntentLayer                `json:"intent"`
	Resolved                     LaunchResolvedLayer              `json:"resolved"`
	RuntimeObservationRelation   RuntimeObservationRelation       `json:"runtime_observation_relation"`
	RuntimeObservation           proto.InstanceRuntimeObservation `json:"runtime_observation"`
	RuntimeObservationReceivedAt *time.Time                       `json:"runtime_observation_received_at,omitempty"`
}

// RuntimeObservationRelation says whether the stored snapshot can be tied to
// the instance's current lifecycle container. Unverified is distinct from
// current: older agents and adopted containers may report a lookup name in the
// lifecycle field, which cannot prove immutable identity.
type RuntimeObservationRelation struct {
	Status string `json:"status"` // current | historical | unverified | unknown
	Reason string `json:"reason,omitempty"`
}

type LaunchIntentLayer struct {
	Status string        `json:"status"`
	Value  *LaunchIntent `json:"value,omitempty"`
}

// LaunchIntent is an allowlisted projection. Deployment fields containing
// arbitrary catalog/source snapshots, residual shell text, or runtime override
// maps do not enter this response.
type LaunchIntent struct {
	DeploymentID     uint              `json:"deployment_id"`
	Name             string            `json:"name"`
	Engine           string            `json:"engine"`
	Image            string            `json:"image,omitempty"`
	ServedName       string            `json:"served_name,omitempty"`
	DesiredState     string            `json:"desired_state"`
	Replicas         int               `json:"replicas"`
	Env              map[string]string `json:"env"`
	Sleepable        bool              `json:"sleepable"`
	DesiredSleep     int               `json:"desired_sleep"`
	VRAMLimitBytes   int64             `json:"vram_limit_bytes"`
	SMLimitPct       int               `json:"sm_limit_pct"`
	PartitionBackend string            `json:"partition_backend,omitempty"`
	NNodes           int               `json:"nnodes,omitempty"`
	NodeRank         int               `json:"node_rank,omitempty"`
}

type LaunchResolvedLayer struct {
	Status string                `json:"status"`
	Value  *proto.ResolvedLaunch `json:"value,omitempty"`
}
