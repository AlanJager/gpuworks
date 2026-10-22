package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A managed benchmark measures the deployment through the gateway unless it
// names one instance (plan D9). The snapshot records which entry it used and,
// for a directly measured PD instance, its role.
func TestBenchmarkEntryDefaultsToGatewayAndNamesInstances(t *testing.T) {
	st, err := store.Open("file:benchmark_entry?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("entry-node", store.HashToken("token"), "entry-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "pd", ServedName: "pd-model", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range []store.Instance{
		{UUID: "pd-prefill", Role: "prefill", APIPort: 31001, GPUIndexes: `[0]`},
		{UUID: "pd-decode", Role: "decode", APIPort: 31002, GPUIndexes: `[1]`},
	} {
		inst.DeploymentID, inst.NodeID, inst.State = dep.ID, node.ID, proto.InstRunning
		if _, err := st.CreateInstance(&inst); err != nil {
			t.Fatal(err)
		}
	}
	signer, err := auth.NewTokenSigner("entry-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	box, _ := auth.NewSecretbox("entry-test")
	h := &apiHandlers{d: Deps{Store: st, AuthSigner: signer, AISecretbox: box, Server: StaticServerSettings{Loopback: "http://127.0.0.1:1"}}, benchmarkExecutions: map[uint]*benchmarkExecution{}}
	create := func(extra string) (int, benchmarkDTO, map[string]any) {
		body := fmt.Sprintf(`{"name":"entry","target_kind":"instance","deployment_id":%d%s,"dataset":{"kind":"random","input_tokens":8,"output_tokens":4},"mode":"grid","search":{"concurrency":[1],"isl":[8],"osl":[4],"requests_per_point_multiplier":1},"allow_conflict":true}`, dep.ID, extra)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.createBenchmark(rec, req)
		var created benchmarkDTO
		_ = json.Unmarshal(rec.Body.Bytes(), &created)
		var snapshot map[string]any
		_ = json.Unmarshal(created.TargetSnapshot, &snapshot)
		if rec.Code != http.StatusAccepted {
			t.Logf("body = %s", rec.Body.String())
		}
		return rec.Code, created, snapshot
	}

	// Nothing named: the gateway, from the server, described by a decode
	// instance, with every GPU of the deployment counted.
	status, created, snapshot := create("")
	if status != http.StatusAccepted || created.LoadOrigin != "server" || created.BaseURL != "http://127.0.0.1:1/api/v1/playground" {
		t.Fatalf("default = %d %+v", status, created)
	}
	if snapshot["entry"] != "gateway" || snapshot["instance_uuid"] != "pd-decode" || snapshot["role"] != nil || created.NumGPUs != 2 {
		t.Fatalf("default snapshot = %v gpus=%d", snapshot, created.NumGPUs)
	}
	// One instance named: that instance, directly, from its node, with its
	// role recorded because the result is not end-to-end.
	status, created, snapshot = create(`,"instance_uuid":"pd-prefill"`)
	if status != http.StatusAccepted || created.LoadOrigin != node.UUID || created.BaseURL != "http://127.0.0.1:31001" || created.InstanceUUID != "pd-prefill" || created.NumGPUs != 1 {
		t.Fatalf("named = %d %+v", status, created)
	}
	if snapshot["entry"] != "instance" || snapshot["role"] != "prefill" {
		t.Fatalf("named snapshot = %v", snapshot)
	}
	// A named instance cannot be measured from the server.
	status, _, _ = create(`,"instance_uuid":"pd-prefill","load_origin":"server"`)
	if status != http.StatusBadRequest {
		t.Fatalf("server origin for an instance = %d", status)
	}
	// An instance that is not serving is refused.
	status, _, _ = create(`,"instance_uuid":"missing"`)
	if status != http.StatusConflict {
		t.Fatalf("missing instance = %d", status)
	}
	// A node origin keeps the direct path to the decode instance there.
	status, created, snapshot = create(fmt.Sprintf(`,"load_origin":%q`, node.UUID))
	if status != http.StatusAccepted || created.BaseURL != "http://127.0.0.1:31002" || snapshot["entry"] != "instance" || snapshot["role"] != "decode" {
		t.Fatalf("node origin = %d %+v %v", status, created, snapshot)
	}
}

func TestBenchmarkGatewayCredentialsRemainRevocable(t *testing.T) {
	for _, credential := range []string{"browser-session", "platform-token"} {
		t.Run(credential, func(t *testing.T) {
			fixture := newBenchmarkAdmission(t, "benchmark-gateway-"+credential)
			dep, _ := fixture.managedTarget(t, "gateway-target")
			fixture.handlers.d.Server = StaticServerSettings{Loopback: "http://127.0.0.1:8800"}
			gatewayCalls := 0
			deps := fixture.handlers.d
			deps.PlaygroundV1 = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				claims, ok := auth.ClaimsFrom(r.Context())
				if !ok || claims.Username != "admin" || claims.Role != "admin" {
					t.Errorf("gateway received the wrong identity: %+v, %v", claims, ok)
				}
				gatewayCalls++
				w.WriteHeader(http.StatusNoContent)
			})
			router := Router(deps)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/benchmarks", nil)
			var revoke func()
			if credential == "browser-session" {
				cookie, _ := sessionLogin(t, router, "password")
				request.AddCookie(cookie)
				revoke = func() {
					if err := fixture.store.SetUserPassword("admin", "new-password"); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				claims, err := deps.AuthSigner.Verify(fixture.token)
				if err != nil {
					t.Fatal(err)
				}
				token, row, err := fixture.store.CreatePlatformToken(claims.UserID, "benchmark", store.PlatformTokenWrite, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+token)
				revoke = func() {
					if _, err := fixture.store.DeletePlatformToken(claims.UserID, row.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			target, failure := fixture.handlers.resolveManagedBenchmarkTarget(request, dep.ID, "", "server")
			if failure != nil || target.apiKey == "" {
				t.Fatalf("resolve gateway credential: target=%+v failure=%+v", target, failure)
			}
			call := func() int {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/v1/chat/completions", strings.NewReader(`{"model":"gateway-target-served"}`))
				req.Header.Set("Authorization", "Bearer "+target.apiKey)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec.Code
			}
			if status := call(); status != http.StatusNoContent || gatewayCalls != 1 {
				t.Fatalf("gateway credential status=%d calls=%d", status, gatewayCalls)
			}
			revoke()
			if status := call(); status != http.StatusUnauthorized || gatewayCalls != 1 {
				t.Fatalf("revoked gateway credential status=%d calls=%d", status, gatewayCalls)
			}
		})
	}
}
