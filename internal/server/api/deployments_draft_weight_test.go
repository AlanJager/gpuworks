package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// A template spec is satisfied by any ready copy of its repository, and the
// deployment's snapshot keeps the provider of the copy it mounted. A model
// imported into a storage reads "local", which no spec declares. The draft
// used to state that provider as the chosen weight source; the field is locked
// in an edit, and the evaluation the edit page runs first refused it
// (ACQUISITION_CANDIDATE_STALE: selected provider is not declared by this
// spec), so such a deployment could not be edited at all - an H20 deployment
// of DeepSeek-V4.1-Flash on a local NFS copy, from a copied template.
func TestDraftOfADeploymentOnALocalCopyStatesNoUndeclaredProvider(t *testing.T) {
	st, h, templateID := templateOperationsFixture(t, "draft_local_copy")
	dep := createTemplateDeployment(t, h, map[string]any{
		"template_id": templateID, "template_spec_name": "embed", "node_uuid": "draft_local_copy-node",
	})

	// A provider the spec declares stays the draft's choice.
	draft, _ := getDraft(t, h, dep.ID)
	if draft.Deployment.ModelSource == nil || draft.Deployment.ModelSource.Provider != "modelscope" {
		t.Fatalf("draft model_source = %+v, want the declared provider modelscope", draft.Deployment.ModelSource)
	}

	// The deployment mounted an imported copy: the snapshot names its provider.
	if err := st.DB().Exec(`UPDATE deployments SET template_snapshot = json_set(template_snapshot, '$.weight_provider', 'local') WHERE id = ?`, dep.ID).Error; err != nil {
		t.Fatal(err)
	}
	draft, raw := getDraft(t, h, dep.ID)
	if draft.Deployment.ModelSource != nil && draft.Deployment.ModelSource.Provider != "" {
		t.Fatalf("draft model_source = %+v, want no provider the spec does not declare", draft.Deployment.ModelSource)
	}
	if strings.Contains(raw, `"provider":"local"`) {
		t.Fatalf("draft wire states the copy's provider: %s", raw)
	}

	// The evaluation the edit page runs accepts the draft as it is.
	body, err := json.Marshal(draft.Deployment)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), ErrorAcquisitionCandidateStale) {
		t.Fatalf("evaluate the draft = %d %s", rec.Code, rec.Body.String())
	}

	// An unchanged draft plans no change, and one that only turns sleep on
	// plans just that.
	status, out := putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: draft.Deployment, BaseRevision: draft.Revision}, true)
	if status != http.StatusOK || len(decodePlan(t, out).Changes) != 0 {
		t.Fatalf("unchanged draft planned = %d %s", status, out)
	}
	edit := draft.Deployment
	edit.Sleepable = true
	status, out = putEdit(t, h, dep.ID, dto.DeploymentEditRequest{Deployment: edit, BaseRevision: draft.Revision}, true)
	if changes := decodePlan(t, out).Changes; status != http.StatusOK || len(changes) != 1 || changes[0].Field != "sleepable" {
		t.Fatalf("sleepable edit planned = %d %s", status, out)
	}
}
