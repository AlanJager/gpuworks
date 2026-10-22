package planning

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
)

// ScheduleRecord is what the controller writes on an instance row when it
// launches it (store.Instance.ScheduleExpl), and the one shape every reader
// takes it back in. Part of it explains the placement to people (the chosen
// plan, the estimate breakdown); part of it is how the instance was launched,
// which later decisions depend on: the compute network it started on (a
// rebuilt peer must share it; the handoff check compares its type) and a PD
// instance's endpoints (the gateway and the multi-node followers reach them).
// It is written once at launch and never changed, so it lives with the
// explanation rather than in columns of its own.
type ScheduleRecord struct {
	Explain   scheduler.Explain  `json:"explain"`
	Breakdown estimate.Breakdown `json:"breakdown"`
	Image     ScheduleImage      `json:"image"`
	PD        *PDRecord          `json:"pd,omitempty"`
	Unit      *UnitRecord        `json:"unit,omitempty"`
	Network   *ScheduleNetwork   `json:"network,omitempty"`
}

// UnitRecord is the unit a leader was launched for: its key (Unit.Key), so a
// failed attempt counts against that unit and no other, and on a multi-node
// unit its follower ranks (Unit.FollowersKey), so the leader is matched to
// that unit's generation.
type UnitRecord struct {
	Key       string `json:"key,omitempty"`
	Followers string `json:"followers,omitempty"`
}

// PDRecord is what a PD instance was launched with: its role, the KV
// endpoint peers and the gateway reach (a bootstrap server for concurrent
// modes) and, on a multi-node unit's leader, the port its ranks meet on.
type PDRecord struct {
	Role    string `json:"role"`
	Address string `json:"address,omitempty"`
	Port    int    `json:"port,omitempty"`
	// MasterPort is the leader's collective port; followers dial it on
	// Address.
	MasterPort int `json:"master_port,omitempty"`
}

// ScheduleImage is the image reference the launch resolved.
type ScheduleImage struct {
	Ref string `json:"ref"`
}

// ScheduleNetwork holds the networks the instance was launched on.
type ScheduleNetwork struct {
	Compute *ComputeNetworkRecord `json:"compute,omitempty"`
}

// ComputeNetworkRecord is the compute network a distributed or PD rank
// advertised itself on.
type ComputeNetworkRecord struct {
	Name       string `json:"name"`
	Management bool   `json:"management"`
	Type       string `json:"type"`
	Interface  string `json:"interface"`
	IP         string `json:"ip"`
	LinkLayer  string `json:"link_layer,omitempty"`
}

// NewScheduleRecord is the record of one launch plan. explainNetwork records
// the rank's compute network, which only distributed and PD ranks advertise;
// unit is set on a multi-node unit's leader.
func NewScheduleRecord(plan LaunchPlan, explainNetwork bool, pd *PDRecord, unit *UnitRecord) ScheduleRecord {
	record := ScheduleRecord{
		Explain:   plan.Output.Result.Explain,
		Breakdown: plan.Output.Breakdown,
		Image:     ScheduleImage{Ref: plan.Source.Image},
		PD:        pd,
		Unit:      unit,
	}
	if topology := plan.Topology; explainNetwork && topology.NetworkName != "" {
		record.Network = &ScheduleNetwork{Compute: &ComputeNetworkRecord{
			Name: topology.NetworkName, Management: topology.NetworkID == 0, Type: topology.Network.Type,
			Interface: topology.Network.Interface, IP: topology.NodeAddress, LinkLayer: topology.Network.LinkLayer,
		}}
	}
	return record
}

// Encode is the column value.
func (r ScheduleRecord) Encode() string {
	raw, _ := json.Marshal(r)
	return string(raw)
}

// ReadScheduleRecord reads a column value; the zero record when there is none
// or it cannot be read.
func ReadScheduleRecord(raw string) ScheduleRecord {
	var record ScheduleRecord
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &record)
	}
	return record
}

// PDRecord returns the PD launch record; the zero record for any other
// instance.
func (r ScheduleRecord) PDRecord() PDRecord {
	if r.PD == nil {
		return PDRecord{}
	}
	return *r.PD
}

// UnitRecord returns the multi-node unit record; the zero record for any
// other instance.
func (r ScheduleRecord) UnitRecord() UnitRecord {
	if r.Unit == nil {
		return UnitRecord{}
	}
	return *r.Unit
}

// ComputeNetwork returns the recorded compute network, if any.
func (r ScheduleRecord) ComputeNetwork() (ComputeNetworkRecord, bool) {
	if r.Network == nil || r.Network.Compute == nil {
		return ComputeNetworkRecord{}, false
	}
	return *r.Network.Compute, true
}
