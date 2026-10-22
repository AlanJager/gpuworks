package cloudprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGiteeLifecycleUsesExactLedgerOwnedID(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	created := false
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/compute/specs":
			_ = json.NewEncoder(w).Encode([]giteeSpec{{Zone: "gitee", GPUModel: "Ascend 910B2C", GPUVRAMGB: 64}})
		case r.Method == http.MethodPost && r.URL.Path == "/compute/instances":
			created = true
			_ = json.NewEncoder(w).Encode([]giteeCreateResult{{ID: "owned-id", Success: true}})
		case r.Method == http.MethodGet && r.URL.Path == "/compute/instances":
			if deleted {
				_ = json.NewEncoder(w).Encode([]giteeInstance{})
				return
			}
			row := giteeInstance{ID: "owned-id", GPUModel: "Ascend 910B2C", GPUCount: 1, Status: "running"}
			row.SSH.Host, row.SSH.Port, row.SSH.User = "127.0.0.1", 22, "root"
			_ = json.NewEncoder(w).Encode([]giteeInstance{row})
		case r.Method == http.MethodPost && r.URL.Path == "/compute/instances/delete":
			if r.URL.Query().Get("ids") != "owned-id" {
				t.Fatalf("delete ids = %q", r.URL.Query().Get("ids"))
			}
			deleted = true
			_ = json.NewEncoder(w).Encode([]giteeCreateResult{{ID: "owned-id", Success: true}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider, err := newGitee("token", "gitee", "image", "run-1", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	instance, err := provider.Rent(context.Background(), GPUSpec{GPUModel: "Ascend 910B2C", GPUCount: 1, MinVRAMMB: 32 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if !created || instance.InstanceID != "owned-id" || instance.Password == "" {
		t.Fatalf("created=%v instance=%+v", created, instance)
	}
	if err := provider.Destroy(context.Background(), "account-owned-id"); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("destroy foreign ID error = %v", err)
	}
	if err := provider.Destroy(context.Background(), instance.InstanceID); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("owned instance was not deleted")
	}
	if leaked, err := provider.ListLeakedForRunID(context.Background(), "run-1"); err != nil || len(leaked) != 0 {
		t.Fatalf("leaked=%v err=%v", leaked, err)
	}
}

func TestGiteeDestroyRetriesTransientDeleteFailure(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	deleteAttempts := 0
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/compute/instances":
			if deleted {
				_ = json.NewEncoder(w).Encode([]giteeInstance{})
				return
			}
			row := giteeInstance{ID: "retry-id", GPUModel: "Ascend 910B2C", GPUCount: 1, Status: "running"}
			_ = json.NewEncoder(w).Encode([]giteeInstance{row})
		case r.Method == http.MethodPost && r.URL.Path == "/compute/instances/delete":
			deleteAttempts++
			if deleteAttempts == 1 {
				http.Error(w, "temporary failure", http.StatusBadGateway)
				return
			}
			deleted = true
			_ = json.NewEncoder(w).Encode([]giteeCreateResult{{ID: "retry-id", Success: true}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if err := addGiteeLedger("run-retry", "retry-id"); err != nil {
		t.Fatal(err)
	}
	provider, err := newGitee("token", "gitee", "image", "run-retry", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Destroy(context.Background(), "retry-id"); err != nil {
		t.Fatal(err)
	}
	if deleteAttempts != 2 {
		t.Fatalf("delete attempts = %d, want 2", deleteAttempts)
	}
	if leaked, err := provider.ListLeakedForRunID(context.Background(), "run-retry"); err != nil || len(leaked) != 0 {
		t.Fatalf("leaked=%v err=%v", leaked, err)
	}
}
