package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInferenceTemplateDetailDeleteCapability(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	builtin := flatAPIFixture("acme/builtin", "base")
	custom := flatAPIFixture("acme/custom", "tuned")
	custom.Source = "user"
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{builtin}); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{custom}, true); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("template-permission-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer})
	session := func(role string) string {
		t.Helper()
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: role, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	admin := session("admin")
	member := session("user")
	claims, err := signer.Verify(admin)
	if err != nil {
		t.Fatal(err)
	}
	platformToken := func(scope string) string {
		t.Helper()
		token, _, err := st.CreatePlatformToken(claims.UserID, scope, scope, nil)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	for _, tc := range []struct {
		name, modelID, token string
		assistant, want      bool
	}{
		{"custom admin", custom.ModelID, admin, false, true},
		{"custom member", custom.ModelID, member, false, false},
		{"upstream admin", builtin.ModelID, admin, false, false},
		{"admin write token", custom.ModelID, platformToken(store.PlatformTokenWrite), false, true},
		{"admin read token", custom.ModelID, platformToken(store.PlatformTokenRead), false, false},
		{"assistant read", custom.ModelID, admin, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/model/"+tc.modelID, nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			if tc.assistant {
				req = req.WithContext(context.WithValue(req.Context(), assistantReadKey{}, true))
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			var detail struct {
				CanDelete *bool `json:"can_delete"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &detail) != nil || detail.CanDelete == nil || *detail.CanDelete != tc.want {
				t.Fatalf("status=%d body=%s want can_delete=%t", rec.Code, rec.Body, tc.want)
			}
		})
	}
}
