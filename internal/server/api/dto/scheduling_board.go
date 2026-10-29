package dto

import "time"

// SchedulingBoard is the scheduling center's view: who holds each GPU, what
// waits to be placed or woken, what the schedules will do next and what the
// automation did last. Nodes is a page of the fleet (page/page_size).
type SchedulingBoard struct {
	Nodes        ListPage[BoardNode]   `json:"nodes"`
	Pending      []BoardPending        `json:"pending"`
	WaitingWakes []BoardWaitingWake    `json:"waiting_wakes"`
	Upcoming     []BoardUpcomingAction `json:"upcoming"`
	Recent       []BoardEvent          `json:"recent"`
}

// BoardNode is one node and its GPUs.
type BoardNode struct {
	UUID   string     `json:"uuid"`
	Name   string     `json:"name"`
	Online bool       `json:"online"`
	GPUs   []BoardGPU `json:"gpus"`
}

// BoardGPU is one GPU as the ledger charges it: AwakeBytes is what awake
// instances claim, SleepingBytes the residue sleepers keep, FreeBytes the
// rest. HeldOutside is a process outside the platform on the card.
type BoardGPU struct {
	Index         int             `json:"index"`
	Name          string          `json:"name"`
	MemoryBytes   int64           `json:"memory_bytes"`
	AwakeBytes    int64           `json:"awake_bytes"`
	SleepingBytes int64           `json:"sleeping_bytes"`
	FreeBytes     int64           `json:"free_bytes"`
	HeldOutside   bool            `json:"held_outside"`
	Occupants     []BoardOccupant `json:"occupants"`
}

// BoardOccupant is one instance on a GPU. State is awake or asleep;
// ResidueBytes is set while asleep.
type BoardOccupant struct {
	DeploymentID uint   `json:"deployment_id"`
	Deployment   string `json:"deployment"`
	Instance     string `json:"instance"`
	State        string `json:"state"`
	SleepLevel   int    `json:"sleep_level,omitempty"`
	ClaimBytes   int64  `json:"claim_bytes"`
	ResidueBytes int64  `json:"residue_bytes,omitempty"`
}

// BoardPending is a deployment the controller could not place lately, with
// its latest reason.
type BoardPending struct {
	DeploymentID uint           `json:"deployment_id"`
	Deployment   string         `json:"deployment"`
	Reason       ScheduleReason `json:"reason"`
	At           time.Time      `json:"at"`
}

// BoardWaitingWake is a deployment's sleepers whose wake waits for room.
type BoardWaitingWake struct {
	DeploymentID uint      `json:"deployment_id"`
	Deployment   string    `json:"deployment"`
	Instances    []string  `json:"instances"`
	Since        time.Time `json:"since"`
}

// BoardUpcomingAction is one occurrence of a schedule in the next 24 hours.
type BoardUpcomingAction struct {
	DeploymentID uint   `json:"deployment_id"`
	Deployment   string `json:"deployment"`
	ScheduleID   uint   `json:"schedule_id"`
	Action       string `json:"action"`
	SleepLevel   int    `json:"sleep_level,omitempty"`
	// Replicas is stated for a replicas order (0 included) and Role names the
	// PD role it scales; both are absent for a sleep or a wake.
	Replicas *int      `json:"replicas,omitempty"`
	Role     string    `json:"role,omitempty"`
	At       time.Time `json:"at"`
}

// BoardEvent is one thing the scheduling center did, from a deployment's
// timeline.
type BoardEvent struct {
	DeploymentID uint           `json:"deployment_id"`
	Deployment   string         `json:"deployment"`
	Code         string         `json:"code"`
	Params       map[string]any `json:"params"`
	At           time.Time      `json:"at"`
}
