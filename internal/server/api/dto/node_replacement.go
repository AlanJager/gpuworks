package dto

// NodeReplacement is how long a node running one of the deployment's
// replicas may stay offline before the replica moves to another node;
// AfterSec 0 never moves it.
type NodeReplacement struct {
	AfterSec int `json:"after_sec"`
}
