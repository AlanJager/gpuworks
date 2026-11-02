package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeletingDeploymentRefusesLifecycleRequests(t *testing.T) {
	for _, tc := range []struct {
		name, action, body, code string
	}{
		{"stop", "stop", "", ErrorDeploymentDeleting},
		{"start", "start", "", ErrorDeploymentDeleting},
		{"restart_all", "restart", `{"strategy":"all"}`, ErrorDeploymentNotActive},
		{"restart_rolling", "restart", `{"strategy":"rolling"}`, ErrorDeploymentNotActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st, dep := createPDForOps(t)
			if rec := pdOpsRequest(h.deleteDeployment, http.MethodDelete, dep.ID, ""); rec.Code != http.StatusAccepted {
				t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
			}
			handlers := map[string]http.HandlerFunc{"stop": h.stopDeployment, "start": h.startDeployment, "restart": h.restartDeployment}
			rec := pdOpsRequest(handlers[tc.action], http.MethodPost, dep.ID, tc.body)
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Errorf("%s = %d %s", tc.name, rec.Code, rec.Body.String())
			}
			if got := mustDeployment(t, st, dep.ID).State; got != store.DeployDeleting {
				t.Fatalf("accepted deletion became %s", got)
			}
		})
	}
}

func TestDeploymentStopThenDeleteIsIdempotent(t *testing.T) {
	h, st, dep := createPDForOps(t)
	before := mustDeployment(t, st, dep.ID)
	for range 2 {
		if rec := pdOpsRequest(h.stopDeployment, http.MethodPost, dep.ID, ""); rec.Code != http.StatusOK {
			t.Fatalf("stop = %d %s", rec.Code, rec.Body.String())
		}
	}
	stopped := mustDeployment(t, st, dep.ID)
	if stopped.State != store.DeployStopped || stopped.Placement != before.Placement || stopped.Engine != before.Engine {
		t.Fatalf("stop lost configuration: before=%+v after=%+v", before, stopped)
	}
	for range 2 {
		if rec := pdOpsRequest(h.deleteDeployment, http.MethodDelete, dep.ID, ""); rec.Code != http.StatusAccepted {
			t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
		}
	}
	if got := mustDeployment(t, st, dep.ID).State; got != store.DeployDeleting {
		t.Fatalf("accepted deletion became %s", got)
	}
}

func TestStopDeploymentMissingIsNotFound(t *testing.T) {
	h, _, dep := createPDForOps(t)
	if rec := pdOpsRequest(h.stopDeployment, http.MethodPost, dep.ID+1, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing stop = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteDeploymentReportsRemovalBeforeStateWrite(t *testing.T) {
	h, st, dep := createPDForOps(t)
	removed := false
	name := "test:remove_before_delete_state_write"
	if err := st.DB().Callback().Update().Before("gorm:begin_transaction").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table != "deployments" || removed {
			return
		}
		removed = true
		if err := st.DeleteDeployment(dep.ID); err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer st.DB().Callback().Update().Remove(name)
	rec := pdOpsRequest(h.deleteDeployment, http.MethodDelete, dep.ID, "")
	if !removed || rec.Code != http.StatusNotFound {
		t.Fatalf("removed=%v delete=%d %s", removed, rec.Code, rec.Body.String())
	}
}

func TestDeploymentConcurrentStartStopDeletePreservesDeletion(t *testing.T) {
	h, st, dep := createPDForOps(t)
	if err := st.SetDeploymentState(dep.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	type result struct {
		name string
		rec  *httptest.ResponseRecorder
	}
	results := make(chan result, 3)
	for _, action := range []struct {
		name, method string
		handler      http.HandlerFunc
	}{
		{"start", http.MethodPost, h.startDeployment},
		{"stop", http.MethodPost, h.stopDeployment},
		{"delete", http.MethodDelete, h.deleteDeployment},
	} {
		go func() {
			<-ready
			results <- result{action.name, pdOpsRequest(action.handler, action.method, dep.ID, "")}
		}()
	}
	close(ready)
	for range 3 {
		got := <-results
		if got.name == "delete" {
			if got.rec.Code != http.StatusAccepted {
				t.Errorf("delete = %d %s", got.rec.Code, got.rec.Body.String())
			}
		} else if got.rec.Code != http.StatusOK && got.rec.Code != http.StatusConflict {
			t.Errorf("%s = %d %s", got.name, got.rec.Code, got.rec.Body.String())
		}
	}
	if got := mustDeployment(t, st, dep.ID).State; got != store.DeployDeleting {
		t.Fatalf("accepted concurrent deletion became %s", got)
	}
}
