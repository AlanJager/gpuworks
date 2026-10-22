package recovery

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// serverDialNode is an offline node the server dials on a provider instance,
// last reached at 203.0.113.158:10034 and logged in to at :10033.
func serverDialNode(t *testing.T, st *store.Store) *store.Node {
	t.Helper()
	node := newOfflineNode(t, st, true)
	if err := st.DB().Model(&store.Node{}).Where("uuid = ?", node.UUID).Updates(map[string]any{"connect_mode": "server_dial", "server_url": ""}).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeServerDialListen(node.UUID, "0.0.0.0:18801"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeServerDialAddress(node.UUID, "203.0.113.158:10034"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeSSHCredential(node.UUID, "203.0.113.158", 10033, "root", store.NodeSSHSecrets{Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.ProviderInstance{ProviderID: 7, ExternalID: "pod-1", NodeID: node.ID}).Error; err != nil {
		t.Fatal(err)
	}
	node, err := st.NodeByUUID(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// A restarted pod: every port renumbered, SSH included.
func renumberedPod() provider.Instance {
	return provider.Instance{ExternalID: "pod-1", PublicIP: "203.0.113.158", SSHPort: 15755, SSHUser: "root", SSHConnections: []provider.SSHConnection{
		{ID: "direct", Type: "direct", Host: "203.0.113.158", Port: 15755, User: "root", InstallSupported: true},
	}}
}

// The provider moved the agent: the node records the new address and the
// server dials it at once, before recovery logs in to the host.
func TestRecoveryFollowsAServerDialAgentTheProviderMoved(t *testing.T) {
	r, st, hub, fake := newRecoveryHarness(t)
	providers := &fakeProviders{instances: []provider.Instance{renumberedPod()}, address: "203.0.113.158:15756"}
	r.providers = providers
	node := serverDialNode(t, st)
	outcome, err := r.recoverNode(context.Background(), node, false)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != "redial" || fake.probes.Load() != 0 {
		t.Fatalf("outcome = %+v, probes = %d", outcome, fake.probes.Load())
	}
	updated, _ := st.NodeByUUID(node.UUID)
	if updated.ServerDialAddress != "203.0.113.158:15756" || updated.ServerDialListen != "0.0.0.0:18801" {
		t.Fatalf("node address = %s listen = %s", updated.ServerDialAddress, updated.ServerDialListen)
	}
	if len(hub.resets) != 1 || hub.resets[0] != node.UUID {
		t.Fatalf("hub resets = %v", hub.resets)
	}
	credential, _, err := st.GetNodeSSHCredential(node.UUID)
	if err != nil || credential.Port != 15755 {
		t.Fatalf("SSH endpoint = %+v, %v; want the renumbered port", credential, err)
	}
}

// Where the agent did not move (or the provider maps no ports) recovery goes
// on as before, through the SSH endpoint the provider lists now.
func TestRecoveryLogsInThroughTheRenumberedSSHEndpoint(t *testing.T) {
	for name, providers := range map[string]*fakeProviders{
		"unchanged address": {instances: []provider.Instance{renumberedPod()}, address: "203.0.113.158:10034"},
		"no port mapping":   {instances: []provider.Instance{renumberedPod()}},
	} {
		t.Run(name, func(t *testing.T) {
			r, st, hub, fake := newRecoveryHarness(t)
			r.providers = providers
			fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: true}
			node := serverDialNode(t, st)
			if _, err := r.recoverNode(context.Background(), node, false); err != nil {
				t.Fatal(err)
			}
			if fake.probes.Load() != 1 || fake.probeReq.Port != 15755 || len(hub.resets) != 0 {
				t.Fatalf("probes = %d at port %d, resets = %v", fake.probes.Load(), fake.probeReq.Port, hub.resets)
			}
			updated, _ := st.NodeByUUID(node.UUID)
			if updated.ServerDialAddress != "203.0.113.158:10034" {
				t.Fatalf("address changed to %s", updated.ServerDialAddress)
			}
		})
	}
}

// A provider that cannot be asked leaves the node as it was; recovery still
// tries the host.
func TestRecoveryKeepsTheAddressWhenTheProviderCannotSay(t *testing.T) {
	r, st, hub, fake := newRecoveryHarness(t)
	r.providers = &fakeProviders{instances: []provider.Instance{renumberedPod()}, addressErr: errors.New("api down")}
	fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: true}
	node := serverDialNode(t, st)
	if _, err := r.recoverNode(context.Background(), node, false); err != nil {
		t.Fatal(err)
	}
	updated, _ := st.NodeByUUID(node.UUID)
	if updated.ServerDialAddress != "203.0.113.158:10034" || len(hub.resets) != 0 || fake.probes.Load() != 1 {
		t.Fatalf("address = %s, resets = %v, probes = %d", updated.ServerDialAddress, hub.resets, fake.probes.Load())
	}
}

// Many offline nodes of one provider account list it once per round.
func TestRecoveryListsAProviderOncePerRound(t *testing.T) {
	r, _, _, _ := newRecoveryHarness(t)
	providers := &fakeProviders{instances: []provider.Instance{renumberedPod()}}
	r.providers = providers
	for i := 0; i < 3; i++ {
		if _, err := r.syncProvider(context.Background(), 7); err != nil {
			t.Fatal(err)
		}
	}
	if providers.calls != 1 {
		t.Fatalf("listed %d times", providers.calls)
	}
}
