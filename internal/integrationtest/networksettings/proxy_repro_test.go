package networksettings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func requestJSON(t *testing.T, env *testenv.Env, api *testenv.APIClient, method, path string, body []byte) map[string]json.RawMessage {
	t.Helper()
	req, err := http.NewRequest(method, env.URL()+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := api.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("%s %s: status=%d body=%s err=%v", method, path, resp.StatusCode, data, err)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// saveProxy makes proxy every node's proxy - one entry for every node - or,
// empty, leaves the nodes with none.
func saveProxy(t *testing.T, env *testenv.Env, api *testenv.APIClient, proxy string) {
	t.Helper()
	listed := requestJSON(t, env, api, http.MethodGet, "/api/v1/pull/proxies", nil)
	var entries []struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(listed["items"], &entries); err != nil {
		t.Fatal(err)
	}
	if proxy == "" {
		for _, entry := range entries {
			requestJSON(t, env, api, http.MethodDelete, fmt.Sprintf("/api/v1/pull/proxies/%d", entry.ID), nil)
		}
	} else {
		body, err := json.Marshal(map[string]string{"http_proxy": proxy, "https_proxy": proxy})
		if err != nil {
			t.Fatal(err)
		}
		requestJSON(t, env, api, http.MethodPost, "/api/v1/pull/proxies", body)
	}
	requestJSON(t, env, api, http.MethodGet, "/api/v1/healthz", nil)
	listed = requestJSON(t, env, api, http.MethodGet, "/api/v1/pull/proxies", nil)
	var got []struct {
		HTTPProxy  string `json:"http_proxy"`
		HTTPSProxy string `json:"https_proxy"`
	}
	if err := json.Unmarshal(listed["items"], &got); err != nil {
		t.Fatal(err)
	}
	if (proxy == "" && len(got) != 0) || (proxy != "" && (len(got) != 1 || got[0].HTTPProxy != proxy || got[0].HTTPSProxy != proxy)) {
		t.Fatalf("stored=%+v want proxy=%q", got, proxy)
	}
}

func rpcWitness(t *testing.T, env *testenv.Env, uuid string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "rpc-witness.txt")
	if err := os.WriteFile(file, []byte("proxy isolation"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := env.Hub().CallNode(ctx, uuid, proto.RPCFSStat, proto.FSStatParams{Path: file})
	if err != nil {
		t.Fatalf("healthy node RPC: %v", err)
	}
	var got proto.FSStatResult
	if err := json.Unmarshal(raw, &got); err != nil || !got.Regular || got.SizeBytes != 15 || got.Path != file {
		t.Fatalf("RPC result=%s err=%v", raw, err)
	}
}

// An unavailable download proxy must not alter management transport routing.
func TestUnreachableGlobalProxyPreservesManagementAndAgentRPC(t *testing.T) {
	for _, count := range []int{1, 3} {
		t.Run(fmt.Sprintf("nodes=%d", count), func(t *testing.T) {
			env := testenv.New(t)
			nodes := make([]*testenv.Node, 0, count)
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("proxy-node-%d", i)
				nodes = append(nodes, env.AddNode(name, testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576})))
				env.WaitNodeOnline(t, name)
			}
			api := env.API(t)
			api.Client.Timeout = 5 * time.Second
			for _, proxy := range []string{"http://replacement.invalid:8080", ""} {
				saveProxy(t, env, api, proxy)
				for _, node := range nodes {
					rpcWitness(t, env, node.UUID)
				}
			}
		})
	}
}
