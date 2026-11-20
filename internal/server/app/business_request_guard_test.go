//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBusinessRequestGuardFollowerQuorumAndLoss(t *testing.T) {
	identity := store.ClusterIdentity{ClusterID: "cluster"}
	var majority atomic.Bool
	majority.Store(true)
	var grants atomic.Int32
	// A follower receives the same Store grant while the majority is live;
	// the hook changes only the result of that fresh grant.
	grant := func(_ context.Context, got store.ClusterIdentity, key string) (uint64, []store.ClusterTransportMember, error) {
		grants.Add(1)
		if got != identity || key != "local-key" {
			t.Fatal("guard changed the bound member identity")
		}
		if !majority.Load() {
			return 0, nil, errors.New("majority unavailable")
		}
		return 2, []store.ClusterTransportMember{
			{ID: 1, KeyFingerprint: "peer-key"},
			{ID: 2, KeyFingerprint: "local-key"},
		}, nil
	}
	var served atomic.Int32
	handler := guardBusinessRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}), identity, 2, "local-key", grant)
	request := func(method, path string, upgrade bool) int {
		r := httptest.NewRequest(method, path, nil)
		if upgrade {
			r.Header.Set("Upgrade", "websocket")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	protected := []struct {
		method, path string
		upgrade      bool
	}{
		{http.MethodPost, "/api/v1/nodes", false},
		{http.MethodPost, "/v1/chat/completions", false},
		{http.MethodGet, "/api/v1/agent/connect", true},
		{http.MethodGet, "/v1/models", false},
		{http.MethodGet, "/api/v1/nodes/node-1/terminal", false},
		{http.MethodPost, "/api/v1/playground/v1/chat/completions", false},
		{http.MethodGet, "/api/v1/nodes", false},
		{http.MethodHead, "/api/v1/nodes", false},
		{http.MethodOptions, "/api/v1/nodes", false},
	}
	for _, tc := range protected {
		if got := request(tc.method, tc.path, tc.upgrade); got != http.StatusNoContent {
			t.Fatalf("quorum request %s %s = %d", tc.method, tc.path, got)
		}
	}
	majority.Store(false)
	for _, tc := range protected {
		if got := request(tc.method, tc.path, tc.upgrade); got != http.StatusServiceUnavailable {
			t.Fatalf("lost-quorum request %s %s = %d", tc.method, tc.path, got)
		}
	}
	if got := request(http.MethodGet, "/healthz", false); got != http.StatusNoContent {
		t.Fatalf("health request = %d", got)
	}
	if got := grants.Load(); got != int32(2*len(protected)) {
		t.Fatalf("grant calls = %d, want one per protected request", got)
	}
	if got := served.Load(); got != int32(len(protected)+1) {
		t.Fatalf("business requests served = %d", got)
	}
}

func TestBusinessRequestGuardRejectsStaleMemberAndKeepsInflightRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var admitted atomic.Bool
	admitted.Store(true)
	grant := func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error) {
		key := "other-key"
		if admitted.Load() {
			key = "local-key"
		}
		return 1, []store.ClusterTransportMember{{ID: 1, KeyFingerprint: key}, {ID: 2, KeyFingerprint: "peer-key"}}, nil
	}
	handler := guardBusinessRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}), store.ClusterIdentity{}, 1, "local-key", grant)
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", nil))
	}()
	<-entered
	admitted.Store(false)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", nil))
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("new request after admission loss = %d", second.Code)
	}
	close(release)
	<-done
	if first.Code != http.StatusNoContent {
		t.Fatalf("in-flight request = %d", first.Code)
	}
}

func TestBusinessRequestGuardWithoutStoreFailsClosed(t *testing.T) {
	handler := guardBusinessRequestsForStore(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("business handler ran without a Store")
	}), nil, store.ClusterIdentity{}, 1, "local-key", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing Store status = %d", w.Code)
	}
}
