package dto

// SleepOrder is the answer to a sleep order: the level and pause mode it
// wrote, and for an order that names instances (one replica, one PD role)
// which ones.
type SleepOrder struct {
	Status    string   `json:"status"`
	Level     int      `json:"level"`
	Mode      string   `json:"mode"`
	Role      string   `json:"role,omitempty"`
	Instances []string `json:"instances,omitempty"`
}

// WakeResult is the answer to a wake order. An order for one replica names
// it; a partial wake lists the sleepers it woke and those whose GPUs could not
// take them back.
type WakeResult struct {
	Status    string        `json:"status"`
	Instances []string      `json:"instances,omitempty"`
	Woken     []string      `json:"woken,omitempty"`
	Refused   []WakeRefusal `json:"refused,omitempty"`
}

// WakeRefusal is sleepers a node's GPUs cannot take back: the GPUs short, by
// how much in total, and who is awake on them. A refused wake carries the same
// fields as its error params.
type WakeRefusal struct {
	Instances    []string `json:"instances"`
	Node         string   `json:"node"`
	GPUIndexes   []int    `json:"gpu_indexes"`
	ShortfallGiB string   `json:"shortfall_gib"`
	Occupants    []string `json:"occupants"`
}
