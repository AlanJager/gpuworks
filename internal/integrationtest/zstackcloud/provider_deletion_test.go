package zstackcloud_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

func TestZStackCloudDeletionRequiresRemovingManagedNode(t *testing.T) {
	cloud := cloudInventoryFixture(t)
	env := newCloudEnv(t, installerFunc(func(_ context.Context, req provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
		if req.Target != "192.0.2.10" || req.Token == "" || req.Password != "ssh-secret" {
			return provision.SSHInstallReport{}, fmt.Errorf("unexpected installation request")
		}
		return provision.SSHInstallReport{Host: req.Target}, nil
	}))
	path := addedProvider(t, env, cloud.URL)
	requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusOK)
	var installed struct {
		NodeUUID string `json:"node_uuid"`
	}
	body := fmt.Sprintf(`{"external_id":"host-a","password":"ssh-secret","connect_mode":"agent_dial","server_url":%q}`, env.URL())
	if err := json.Unmarshal(requestJSON(t, env, "POST", path+"/install", body, http.StatusOK), &installed); err != nil || installed.NodeUUID == "" {
		t.Fatalf("install = %+v, err=%v", installed, err)
	}
	requestJSON(t, env, "DELETE", path, "", http.StatusConflict)
	assertCloudProviderPaths(t, env, path)
	assertDiscoveredCloudHosts(t, env, path, "host-a", "host-b")
	requestJSON(t, env, "GET", "/api/v1/nodes/"+installed.NodeUUID, "", http.StatusOK)
	// Re-sync proves the rejected deletion retained the connection credentials.
	requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusOK)
	// This inventory fixture has no SSH host; explicitly remove only its record.
	requestJSON(t, env, "DELETE", "/api/v1/nodes/"+installed.NodeUUID+"?force=true", "", http.StatusOK)
	requestJSON(t, env, "DELETE", path, "", http.StatusOK)
	assertCloudProviderPaths(t, env)
}

func TestZStackCloudDeletionProtectsLeasesWithoutNodes(t *testing.T) {
	for _, scenario := range []struct {
		state  string
		status int
	}{
		{state: "reserved", status: http.StatusConflict},
		{state: "release_pending", status: http.StatusConflict},
		{state: "unknown", status: http.StatusConflict},
		{state: "failed", status: http.StatusConflict},
		{state: "released", status: http.StatusOK},
	} {
		t.Run(scenario.state, func(t *testing.T) {
			cloud := cloudInventoryFixture(t)
			env := testenv.New(t)
			path := addedProvider(t, env, cloud.URL)
			otherPath := addedProvider(t, env, cloud.URL)
			var providerID uint
			if _, err := fmt.Sscanf(path, "/api/v1/providers/%d", &providerID); err != nil {
				t.Fatal(err)
			}
			// Seed persisted ownership left behind by a missing node. The
			// deletion verdict and credential preservation are checked via API.
			ledger := cloudstore.New(env.Store().DB())
			if err := ledger.SaveCloudHostSnapshot(cloudstore.CloudHost{ProviderID: providerID, ExternalID: "host-a", NodeUUID: "removed-node", Snapshot: `{}`}); err != nil {
				t.Fatal(err)
			}
			host, err := ledger.CloudHostByNodeUUID("removed-node")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ledger.CreateCloudResourceLease(&cloudstore.CloudResourceLease{
				OperationID: "orphaned-deployment/0", ProviderID: providerID, CloudHostID: host.ID,
				NodeUUID: "removed-node", InstanceUUID: "removed-instance",
				ResourceType: "gpu", ResourceID: "gpu-a", Amount: 1, Unit: "device", State: scenario.state,
			}); err != nil {
				t.Fatal(err)
			}
			requestJSON(t, env, "DELETE", otherPath, "", http.StatusOK)
			requestJSON(t, env, "DELETE", path, "", scenario.status)
			if scenario.status == http.StatusOK {
				assertCloudProviderPaths(t, env)
				return
			}
			assertCloudProviderPaths(t, env, path)
			requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusOK)
			assertDiscoveredCloudHosts(t, env, path, "host-a", "host-b")
		})
	}
}

func TestZStackCloudDeletionKeepsConfigurationWhenDependencyCheckFails(t *testing.T) {
	for _, table := range []string{"cloud_hosts", "cloud_resource_leases"} {
		t.Run(table, func(t *testing.T) {
			cloud := cloudInventoryFixture(t)
			env := testenv.New(t)
			path := addedProvider(t, env, cloud.URL)
			requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusOK)
			// Make the dependency query fail against the real database, without
			// replacing the store or changing the provider's saved credentials.
			func() {
				migrator := env.Store().DB().Migrator()
				unavailable := table + "_unavailable"
				if err := migrator.RenameTable(table, unavailable); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := migrator.RenameTable(unavailable, table); err != nil {
						t.Errorf("restore dependency table: %v", err)
					}
				}()
				requestJSON(t, env, "DELETE", path, "", http.StatusInternalServerError)
			}()
			assertCloudProviderPaths(t, env, path)
			assertDiscoveredCloudHosts(t, env, path, "host-a", "host-b")
			requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusOK)
			requestJSON(t, env, "DELETE", path, "", http.StatusOK)
			assertCloudProviderPaths(t, env)
		})
	}
}

func TestPublicCloudConfigurationDeletionDoesNotRequireValidCredentials(t *testing.T) {
	env := testenv.New(t)
	// Existing drivers without a deletion check must keep local deletion
	// available even when their credentials cannot be decrypted or used.
	p, err := env.Store().CreateProvider(&store.Provider{
		Name: "existing public cloud", Kind: "compshare", State: "invalid", CredsCipher: "unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	requestJSON(t, env, "DELETE", fmt.Sprintf("/api/v1/providers/%d", p.ID), "", http.StatusOK)
	assertCloudProviderPaths(t, env)
}

func TestZStackCloudDeletionRemovesOnlySelectedInventory(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		offline bool
	}{
		{name: "reachable Cloud"},
		{name: "unreachable Cloud", offline: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			cloud := cloudInventoryFixture(t)
			otherCloud := cloudInventoryFixture(t)
			env := testenv.New(t)
			path := addedProvider(t, env, cloud.URL)
			otherPath := addedProvider(t, env, otherCloud.URL)
			for _, providerPath := range []string{path, otherPath} {
				requestJSON(t, env, "POST", providerPath+"/sync", `{}`, http.StatusOK)
				assertDiscoveredCloudHosts(t, env, providerPath, "host-a", "host-b")
			}
			assertCloudProviderPaths(t, env, path, otherPath)
			if scenario.offline {
				cloud.Close()
			}

			var deleted struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(requestJSON(t, env, "DELETE", path, "", http.StatusOK), &deleted); err != nil || deleted.Status != "deleted" {
				t.Fatalf("delete response = %+v, err=%v", deleted, err)
			}
			assertCloudProviderPaths(t, env, otherPath)
			assertDiscoveredCloudHosts(t, env, path)
			assertDiscoveredCloudHosts(t, env, otherPath, "host-a", "host-b")
			requestJSON(t, env, "PUT", path, `{"name":"deleted Cloud"}`, http.StatusNotFound)

			// Retrying a completed deletion must not affect the remaining Cloud.
			requestJSON(t, env, "DELETE", path, "", http.StatusOK)
			requestJSON(t, env, "POST", otherPath+"/sync", `{}`, http.StatusOK)
			assertCloudProviderPaths(t, env, otherPath)
			assertDiscoveredCloudHosts(t, env, otherPath, "host-a", "host-b")
		})
	}
}

func TestZStackCloudDeletionRequiresAuthentication(t *testing.T) {
	cloud := cloudInventoryFixture(t)
	env := testenv.New(t)
	path := addedProvider(t, env, cloud.URL)
	requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusOK)
	for _, scenario := range []struct {
		name          string
		authorization string
	}{
		{name: "anonymous"},
		{name: "invalid token", authorization: "Bearer invalid-test-token"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, env.URL()+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.authorization != "" {
				req.Header.Set("Authorization", scenario.authorization)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("unauthenticated deletion status = %d, want 401", resp.StatusCode)
			}
			assertCloudProviderPaths(t, env, path)
			assertDiscoveredCloudHosts(t, env, path, "host-a", "host-b")
		})
	}
	requestJSON(t, env, "POST", path+"/sync", `{}`, http.StatusOK)
	assertDiscoveredCloudHosts(t, env, path, "host-a", "host-b")
}

func TestZStackCloudCanBeAddedAgainAfterDeletion(t *testing.T) {
	cloud := cloudInventoryFixture(t)
	env := testenv.New(t)
	path := addedProvider(t, env, cloud.URL)
	assertCloudProviderPaths(t, env, path)
	assertDiscoveredCloudHosts(t, env, path)
	requestJSON(t, env, "DELETE", path, "", http.StatusOK)
	assertCloudProviderPaths(t, env)

	recreatedPath := addedProvider(t, env, cloud.URL)
	assertCloudProviderPaths(t, env, recreatedPath)
	assertDiscoveredCloudHosts(t, env, recreatedPath)
	requestJSON(t, env, "POST", recreatedPath+"/sync", `{}`, http.StatusOK)
	assertDiscoveredCloudHosts(t, env, recreatedPath, "host-a", "host-b")
}

func assertCloudProviderPaths(t *testing.T, env *testenv.Env, want ...string) {
	t.Helper()
	var providers []struct {
		ID   uint   `json:"id"`
		Kind string `json:"kind"`
	}
	if err := apiwire.UnmarshalList(requestJSON(t, env, "GET", "/api/v1/providers", "", http.StatusOK), &providers); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(providers))
	for _, p := range providers {
		if p.Kind != "zstack" {
			t.Fatalf("unexpected provider kind: %q", p.Kind)
		}
		got = append(got, fmt.Sprintf("/api/v1/providers/%d", p.ID))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Cloud providers = %v, want %v", got, want)
	}
}

func assertDiscoveredCloudHosts(t *testing.T, env *testenv.Env, path string, want ...string) {
	t.Helper()
	var instances []struct {
		ExternalID string `json:"external_id"`
	}
	if err := apiwire.UnmarshalList(requestJSON(t, env, "GET", path+"/instances", "", http.StatusOK), &instances); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(instances))
	for _, instance := range instances {
		got = append(got, instance.ExternalID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s hosts = %v, want %v", path, got, want)
	}
}
