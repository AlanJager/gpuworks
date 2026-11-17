package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentNodeFilterUsesRuntimePresenceBeforePaging(t *testing.T) {
	f := newListFixture(t, 0)
	options := []dto.DeploymentNodeOption{{UUID: "list-a", Name: "list-a"}, {UUID: "list-b", Name: "list-b"}}
	for _, tc := range []struct {
		query string
		names []string
		total int
	}{
		{"node=list-a", []string{"multi", "single"}, 2},
		{"node=list-b", []string{"multi", "scheduled-away"}, 2},
		{"node=list-a,list-b", []string{"multi", "scheduled-away", "single"}, 3},
		{"node=list-b&q=scheduled", []string{"scheduled-away"}, 1},
		{"node=list-b&page_size=1&page=2", []string{"scheduled-away"}, 2},
		{"node=missing", []string{}, 0},
		{"node=list-b&state=failed", []string{}, 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			page := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?"+tc.query+"&page=1&sort=name"))
			if !reflect.DeepEqual(pageNames(page), tc.names) || page.Total != tc.total {
				t.Fatalf("names=%v total=%d", pageNames(page), page.Total)
			}
			if !reflect.DeepEqual(page.NodeOptions, options) {
				t.Fatalf("node options=%v", page.NodeOptions)
			}
		})
	}
	for _, query := range []string{"node=list-a,", "node=,list-a"} {
		rec := httptest.NewRecorder()
		f.handler.listDeployments(rec, httptest.NewRequest(http.MethodGet, "/api/v1/deployments?page=1&"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d", query, rec.Code)
		}
	}
}

func TestDeploymentNodeFilterRetainsFailedInstances(t *testing.T) {
	f := newListFixture(t, 0)
	if err := f.st.DB().Model(&store.Instance{}).Where("uuid = ?", "inst-scheduled-away").Update("state", proto.InstFailed).Error; err != nil {
		t.Fatal(err)
	}
	page := decodeDeploymentPage(t, f.get(t, f.handler.listDeployments, "/api/v1/deployments?page=1&node=list-b&state=failed"))
	if !reflect.DeepEqual(pageNames(page), []string{"scheduled-away"}) {
		t.Fatalf("failed on actual node: %v", pageNames(page))
	}
}
