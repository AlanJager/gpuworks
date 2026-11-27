package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// DELETE /models/{id} refused because deployments name the model says which,
// and whether they can all go with it; with delete_stopped_deployments the
// stopped, empty ones are deleted together with the model (GWKS-530).
func TestDeletingAModelListsTheDeploymentsThatKeepIt(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	model, err := rig.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/kept", Revision: "main", Name: "kept"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := rig.st.CreateDeployment(&store.Deployment{Name: "shell", ServedName: "shell", ModelID: model.ID, State: store.DeployStopped})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/models/%d", model.ID)

	rec := rig.do(rig.session, http.MethodDelete, path, "")
	want := fmt.Sprintf(`{"code":"MODEL_IN_USE","error":"The model is referenced by a deployment. Delete the related deployments first.","params":{"count":1,"deletable_with_deployments":true,"deployments":[{"id":%d,"instances":0,"name":"shell","state":"stopped"}]}}`, dep.ID)
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("refusal = %d %s\nwant %s", rec.Code, rec.Body, want)
	}
	for _, bad := range []string{"?delete_stopped_deployments=maybe", "?delete_stopped_deployments=true&delete_stopped_deployments=false"} {
		if got := rig.do(rig.session, http.MethodDelete, path+bad, ""); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), `"field":"delete_stopped_deployments"`) {
			t.Fatalf("%s = %d %s", bad, got.Code, got.Body)
		}
	}
	if got := rig.do("", http.MethodDelete, path+"?delete_stopped_deployments=true", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", got.Code)
	}
	if got := rig.do(rig.session, http.MethodDelete, path+"?delete_stopped_deployments=true", ""); got.Code != http.StatusNoContent {
		t.Fatalf("delete with deployments = %d %s", got.Code, got.Body)
	}
	if _, err := rig.st.GetDeployment(dep.ID); err == nil {
		t.Fatal("the stopped deployment stayed")
	}
	if got := rig.do(rig.session, http.MethodDelete, path, ""); got.Code != http.StatusNotFound {
		t.Fatalf("deleting it again = %d", got.Code)
	}
}
