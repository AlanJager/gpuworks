package api

import (
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

// settingsFencingRouter is the real router over a file-backed store, with an
// administrator session and a viewer session to send requests as.
func settingsFencingRouter(t *testing.T) (*store.Store, http.Handler, string, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, err := auth.NewTokenSigner("settings-fencing", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "viewer", Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	return st, Router(Deps{Store: st, Bus: st.Bus, AuthSigner: signer}), admin, viewer
}

func settingsFencingRequest(t *testing.T, router http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}
