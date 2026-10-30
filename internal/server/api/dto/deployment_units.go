package dto

// UnitSuggestRequest asks for the nodes of one more multi-node replica
// (POST /deployments/{id}/units/suggest), given the replicas listed so far
// in the editor: their leaders cannot lead it, and the cards of the ones not
// yet running are counted as taken.
type UnitSuggestRequest struct {
	Units []UnitInput `json:"units"`
}

// UnitInput is one listed multi-node replica: its leader node, then its
// followers' nodes rank by rank.
type UnitInput struct {
	NodeUUID  string          `json:"node_uuid"`
	Followers []UnitRankInput `json:"followers,omitempty"`
}

// UnitRankInput is one follower rank's node.
type UnitRankInput struct {
	NodeUUID string `json:"node_uuid"`
}

// UnitSuggestion is a proposed replica for the operator to confirm: Unit is
// its leader then followers, on nodes that share Network and one GPU
// architecture (Arch, vendor/family), each rank trial-planned. Unit is nil
// when no group fits; Reason then says why (unit_none with the counts of
// nodes each check left out, or unit_no_fit with the first refusal).
type UnitSuggestion struct {
	Unit    *UnitInput              `json:"unit"`
	Network *DeploymentNetworkBrief `json:"network,omitempty"`
	Arch    string                  `json:"arch,omitempty"`
	Nodes   []UnitSuggestedNode     `json:"nodes,omitempty"`
	Reason  *ScheduleReason         `json:"reason,omitempty"`
}

// DeploymentNetworkBrief names a network.
type DeploymentNetworkBrief struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// UnitSuggestedNode is one rank's node in a suggestion, rank order.
type UnitSuggestedNode struct {
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	FreeGPUs int    `json:"free_gpus"`
	Model    string `json:"model"`
}

// PlacementModeRequest switches a multi-node or PD deployment between
// automatic placement ("auto") and the operator's own ("manual").
type PlacementModeRequest struct {
	Mode string `json:"mode"`
}
