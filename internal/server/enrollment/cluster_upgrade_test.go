package enrollment_test

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// The cluster upgrade command of a separately built server reads this answer
// and names these steps. The wire names are pinned literally.
func TestClusterUpgradeCommandReadsTheStepAnswer(t *testing.T) {
	for _, step := range []string{enrollment.ClusterUpgradeStatus, enrollment.ClusterUpgradeBegin, enrollment.ClusterUpgradeMigrate, enrollment.ClusterUpgradeFinish, enrollment.ClusterUpgradeAbort} {
		if !enrollment.ValidClusterUpgradeAction(step) {
			t.Fatalf("step %q is not accepted", step)
		}
	}
	if enrollment.ValidClusterUpgradeAction("restart") || enrollment.ClusterUpgradePath != "/_cluster/upgrade" {
		t.Fatal("the step names or the path changed")
	}
	wire := `{"action":"migrate","leader":true,"phase":"upgrading","phase_revision":5,"applied":245,"known":246,"applied_extensions":5,"known_extensions":6,"applied_by_extension":{"license":5},"base":245,"ready":true,"migrating":true,"members":[{"address":"192.0.2.1:9001","reported":true,"markers":246,"last":"drop_node_placement_policy","extensions":6,"fresh":true}],"refused":"r","error":"e"}`
	var response enrollment.ClusterUpgradeResponse
	if err := json.Unmarshal([]byte(wire), &response); err != nil {
		t.Fatal(err)
	}
	if response.Action != "migrate" || !response.Leader || response.Phase != "upgrading" || response.PhaseRevision != 5 ||
		response.Applied != 245 || response.Known != 246 || response.AppliedExtensions != 5 || response.KnownExtensions != 6 ||
		len(response.AppliedByExtension) != 1 || response.AppliedByExtension["license"] != 5 ||
		response.Base != 245 || !response.Ready || !response.Migrating || response.Refused != "r" || response.Error != "e" ||
		len(response.Members) != 1 || response.Members[0] != (enrollment.ClusterUpgradeMember{Address: "192.0.2.1:9001", Reported: true, Markers: 246, Last: "drop_node_placement_policy", Extensions: 6, Fresh: true}) {
		t.Fatalf("decoded %+v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil || string(encoded) != wire {
		t.Fatalf("round trip = %s, %v\nwant        %s", encoded, err, wire)
	}
	quiet, err := json.Marshal(enrollment.ClusterUpgradeResponse{Action: "status"})
	if err != nil || string(quiet) != `{"action":"status","leader":false,"phase":"","phase_revision":0,"applied":0,"known":0,"applied_extensions":0,"known_extensions":0,"base":0,"ready":false,"migrating":false,"members":null}` {
		t.Fatalf("an empty answer = %s, %v", quiet, err)
	}
}
