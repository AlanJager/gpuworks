package api

import (
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// An edit cannot turn sleep off under a yield policy that gives way: the
// deployment would keep sharing its cards (store.SharingDeployments), new
// replicas would wait for it, and its sleep would be refused forever. It is
// refused by name, as a create with that policy and no sleep is, and turning
// sleep off once the policy is never goes through.
func TestEditCannotTurnSleepOffUnderAYieldPolicy(t *testing.T) {
	h, st := automaticPlacementFixture(t)
	dep := editableDeployment(t, h, automaticPlacementModel(t, st))
	if err := st.DB().Model(&store.Deployment{}).Where("id = ?", dep.ID).Update("sleepable", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentYieldPolicy(dep.ID, yieldpolicy.Defaults(yieldpolicy.Idle)); err != nil {
		t.Fatal(err)
	}
	draft, _ := getDraft(t, h, dep.ID)
	if !draft.Deployment.Sleepable {
		t.Fatal("the draft does not carry the deployment's sleep")
	}
	off := draft.Deployment
	off.Sleepable = false
	for _, dryRun := range []bool{true, false} {
		status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: off, BaseRevision: draft.Revision}, dryRun)
		if code, params := errorBody(t, body); status != http.StatusBadRequest || code != messages.ErrorYieldPolicyNeedsSleep || params["policy"] != yieldpolicy.Idle {
			t.Fatalf("dry run %v: turning sleep off under idle = %d %s", dryRun, status, body)
		}
	}
	if row, _ := st.GetDeployment(dep.ID); !row.Sleepable || row.YieldPolicy != yieldpolicy.Idle {
		t.Fatalf("a refused edit wrote sleepable=%v policy=%s", row.Sleepable, row.YieldPolicy)
	}
	// Keeping sleep on under the policy is no refusal.
	if status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: draft.Revision}, true); status != http.StatusOK {
		t.Fatalf("an unchanged edit under idle = %d %s", status, body)
	}
	// Once the policy is never, sleep may go.
	if err := st.SetDeploymentYieldPolicy(dep.ID, yieldpolicy.Policy{Yield: yieldpolicy.Never}); err != nil {
		t.Fatal(err)
	}
	draft, _ = getDraft(t, h, dep.ID)
	off = draft.Deployment
	off.Sleepable = false
	if status, body := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: off, BaseRevision: draft.Revision}, false); status != http.StatusOK || strings.Contains(body, messages.ErrorYieldPolicyNeedsSleep) {
		t.Fatalf("turning sleep off under never = %d %s", status, body)
	}
	if row, _ := st.GetDeployment(dep.ID); row.Sleepable {
		t.Fatal("sleep is still on")
	}
}
