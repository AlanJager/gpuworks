package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ClusterUpgradePath is the member-authenticated route for the steps of a
// cluster upgrade. It is served by the member's own enrollment listener,
// which answers while the member's business is closed.
const ClusterUpgradePath = "/_cluster/upgrade"

// The steps of a cluster upgrade. Status and report change nothing a later
// step depends on; the others act only on the database leader.
const (
	ClusterUpgradeStatus  = "status"
	ClusterUpgradeBegin   = "begin"
	ClusterUpgradeMigrate = "migrate"
	ClusterUpgradeFinish  = "finish"
	ClusterUpgradeAbort   = "abort"
)

// ClusterUpgradeRequest names one step.
type ClusterUpgradeRequest struct {
	Action string `json:"action"`
}

// ClusterUpgradeMember is one admitted member and the migration lists its
// executable reported; Reported is false for one that has not reported.
// Markers and Last are the core list's.
type ClusterUpgradeMember struct {
	Address  string `json:"address"`
	Reported bool   `json:"reported"`
	Markers  int    `json:"markers"`
	Last     string `json:"last"`
	// Extensions counts the markers of the member's extension modules.
	Extensions int `json:"extensions"`
	// Fresh is false for a member that has stopped repeating its report.
	Fresh bool `json:"fresh"`
}

// ClusterUpgradeResponse is the cluster's upgrade state after the step.
// Refused carries the reason a step did not run; Error a step that failed.
// Leader says whether the answering member is the database leader, the only
// member on which begin, migrate, finish and abort act.
type ClusterUpgradeResponse struct {
	Action        string `json:"action"`
	Leader        bool   `json:"leader"`
	Phase         string `json:"phase"`
	PhaseRevision int64  `json:"phase_revision"`
	Applied       int    `json:"applied"`
	Known         int    `json:"known"`
	// The extension pair counts the markers of the answering executable's
	// extension modules: applied to the database, and known to it.
	AppliedExtensions int                    `json:"applied_extensions"`
	KnownExtensions   int                    `json:"known_extensions"`
	Base              int                    `json:"base"`
	Ready             bool                   `json:"ready"`
	Migrating         bool                   `json:"migrating"`
	Members           []ClusterUpgradeMember `json:"members"`
	Refused           string                 `json:"refused,omitempty"`
	Error             string                 `json:"error,omitempty"`
}

// ValidClusterUpgradeAction reports a step this protocol knows.
func ValidClusterUpgradeAction(action string) bool {
	switch action {
	case ClusterUpgradeStatus, ClusterUpgradeBegin, ClusterUpgradeMigrate, ClusterUpgradeFinish, ClusterUpgradeAbort:
		return true
	}
	return false
}

// ClusterUpgrade asks the member behind this client for one upgrade step,
// authenticated with the caller's member certificate.
func (c *Client) ClusterUpgrade(ctx context.Context, certificate, key []byte, action string) (ClusterUpgradeResponse, error) {
	if !ValidClusterUpgradeAction(action) {
		return ClusterUpgradeResponse{}, errors.New("unknown cluster upgrade step")
	}
	body, err := json.Marshal(ClusterUpgradeRequest{Action: action})
	if err != nil {
		return ClusterUpgradeResponse{}, err
	}
	data, _, err := c.memberRequestForPeerWithBodyAndKey(ctx, certificate, key, ClusterUpgradePath, body, 256<<10, "", http.StatusOK)
	if err != nil {
		return ClusterUpgradeResponse{}, err
	}
	var response ClusterUpgradeResponse
	if err := json.Unmarshal(data, &response); err != nil || response.Action != action {
		return ClusterUpgradeResponse{}, errors.New("cluster upgrade response rejected")
	}
	return response, nil
}
