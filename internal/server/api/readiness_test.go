package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestReadinessRouteAnonymousReadSuccessAndFailures(t *testing.T) {
	for _, mode := range []string{"healthy", "bad-settings-table", "bad-node-table", "bad-auth-table", "bad-settings-document", "closed-sql", "no-signer", "no-store"} {
		t.Run(mode, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			signer, err := auth.NewTokenSigner("ready-route-secret", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.DB().Create(&store.Setting{Key: "private-setting-key", Value: `{"value":"private-setting-value"}`}).Error; err != nil {
				t.Fatal(err)
			}
			deps := Deps{Store: st, AuthSigner: signer}
			switch mode {
			case "bad-settings-table", "bad-node-table", "bad-auth-table":
				table := map[string]string{"bad-settings-table": "settings", "bad-node-table": "nodes", "bad-auth-table": "users"}[mode]
				if err := st.DB().Exec("DROP TABLE " + table).Error; err != nil {
					t.Fatal(err)
				}
			case "bad-settings-document":
				if err := st.DB().Model(&store.Setting{}).Where("key = ?", "private-setting-key").UpdateColumn("value", "private-malformed-json").Error; err != nil {
					t.Fatal(err)
				}
			case "closed-sql":
				pool, err := st.DB().DB()
				if err != nil {
					t.Fatal(err)
				}
				if err := pool.Close(); err != nil {
					t.Fatal(err)
				}
			case "no-signer":
				deps.AuthSigner = nil
			case "no-store":
				deps.Store = nil
			}
			router := Router(deps)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
			want, body := http.StatusServiceUnavailable, "{\"code\":\"SERVICE_UNAVAILABLE\",\"error\":\"business service is not ready\"}\n"
			if mode == "healthy" {
				want, body = http.StatusOK, "{\"status\":\"ready\"}\n"
			}
			if rec.Code != want || rec.Body.String() != body || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response=%d %s headers=%v", rec.Code, rec.Body.String(), rec.Header())
			}
			for _, private := range []string{"private-setting", "users", "SELECT", st.DB().Name()} {
				if private != "" && strings.Contains(rec.Body.String(), private) {
					t.Fatalf("readiness leaked %q", private)
				}
			}
			// Only GET is anonymous. Any other method falls to the management
			// routes, which answer an anonymous caller 401 before 405.
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodHead} {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/readyz", nil))
				if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "ready") {
					t.Fatalf("%s readyz=%d %s", method, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

// A caller with a session gets the same fixed answer as an anonymous probe:
// the route carries no identity.
func TestReadinessRouteIgnoresCredentials(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, bearer := range []string{"", rig.session, "not-a-token"} {
		rec := rig.do(bearer, http.MethodGet, "/api/v1/readyz", "")
		if rec.Code != http.StatusOK || rec.Body.String() != "{\"status\":\"ready\"}\n" {
			t.Fatalf("bearer %q: %d %q", bearer, rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []struct {
		bearer, method, path string
		want                 int
	}{
		{rig.session, http.MethodPost, "/api/v1/readyz", http.StatusMethodNotAllowed},
		{rig.session, http.MethodDelete, "/api/v1/readyz", http.StatusMethodNotAllowed},
		{rig.session, http.MethodGet, "/api/v1/readyz/extra", http.StatusNotFound},
		{"", http.MethodGet, "/api/v1/readyz/extra", http.StatusUnauthorized},
	} {
		if rec := rig.do(tc.bearer, tc.method, tc.path, ""); rec.Code != tc.want {
			t.Fatalf("%s %s authenticated=%t: %d, want %d", tc.method, tc.path, tc.bearer != "", rec.Code, tc.want)
		}
	}
}

// A closed member and a follower asked for a leader-only write both answer
// 503, so the cluster entry tries the next member.
func TestWriteErrAnswersMemberRefusalsWithServiceUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
		want int
	}{
		{"closed business writes", store.ErrBusinessWritesClosed, http.StatusInternalServerError, http.StatusServiceUnavailable},
		{"wrapped closed business writes", fmt.Errorf("save setting: %w", store.ErrBusinessWritesClosed), http.StatusBadRequest, http.StatusServiceUnavailable},
		{"stale scheduling generation", fmt.Errorf("commit: %w", store.ErrStaleSchedulingGeneration), http.StatusInternalServerError, http.StatusServiceUnavailable},
		{"ordinary failure keeps its status", errors.New("boom"), http.StatusInternalServerError, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeErr(rec, tc.code, tc.err)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["error"] != tc.err.Error() || body["code"] == "" {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}
}
