package dto

import "time"

// SchedulingTimeline is the scheduling center's residency timeline for some
// nodes: for each GPU, which deployment's instance held it and in what state
// over a window ending now, and the stats of the deployments that share cards.
type SchedulingTimeline struct {
	Window      TimelineWindow       `json:"window"`
	Nodes       []TimelineNode       `json:"nodes"`
	Deployments []TimelineDeployment `json:"deployments"`
}

// TimelineWindow is the stretch of time the timeline covers: Hours long,
// ending at To.
type TimelineWindow struct {
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
	Hours int       `json:"hours"`
}

// TimelineNode is one node and its GPUs' timelines.
type TimelineNode struct {
	UUID string        `json:"uuid"`
	Name string        `json:"name"`
	GPUs []TimelineGPU `json:"gpus"`
}

// TimelineGPU is one GPU's segments, oldest first. Truncated says older
// segments were left out to keep the answer bounded.
type TimelineGPU struct {
	Index     int               `json:"index"`
	Segments  []TimelineSegment `json:"segments"`
	Truncated bool              `json:"truncated"`
}

// TimelineSegment is one stretch of an instance holding a GPU in one state:
// awake, asleep (Level says how deep), switching (between a sleep or wake
// order and its result) or starting. From and To are inside the window.
type TimelineSegment struct {
	DeploymentID uint      `json:"deployment_id"`
	Deployment   string    `json:"deployment"`
	Instance     string    `json:"instance"`
	State        string    `json:"state"`
	Level        int       `json:"level,omitempty"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
}

// TimelineDeployment is what a deployment that shares GPUs (any yield policy
// but never) did on the listed nodes over the window. AwakeSeconds is the
// time at least one of its instances there was awake and AwakeShare that over
// the window (0 to 1). Yielded counts the times it was put to sleep for
// another deployment, YieldedTo those another was put to sleep for it, Wakes
// those the platform woke it, and Switches is Yielded plus Wakes.
// HeldRequests counts the requests the gateway held while it woke; the waits
// are approximated from the gateway's per-interval quantiles weighted by the
// requests of each interval and are absent when no request was held or the
// gateway's metrics are not kept.
type TimelineDeployment struct {
	DeploymentID uint   `json:"deployment_id"`
	Deployment   string `json:"deployment"`
	// YieldPolicy is its yield policy (never, idle, timeslice, anytime).
	YieldPolicy  string   `json:"yield_policy"`
	AwakeSeconds int64    `json:"awake_seconds"`
	AwakeShare   float64  `json:"awake_share"`
	Switches     int      `json:"switches"`
	Yielded      int      `json:"yielded"`
	YieldedTo    int      `json:"yielded_to"`
	Wakes        int      `json:"wakes"`
	HeldRequests int      `json:"held_requests"`
	WaitP50Ms    *float64 `json:"wait_p50_ms,omitempty"`
	WaitP95Ms    *float64 `json:"wait_p95_ms,omitempty"`
	// Cards is where its live instances are now, by node; empty when none
	// is placed.
	Cards []TimelineCards `json:"cards"`
}

// TimelineCards is the GPUs a deployment's instances hold on one node now;
// Node is empty for a node no longer known, and so is NodeUUID, by which
// the UI opens the node.
type TimelineCards struct {
	NodeID   uint   `json:"node_id"`
	NodeUUID string `json:"node_uuid,omitempty"`
	Node     string `json:"node"`
	GPUs     []int  `json:"gpus"`
}
