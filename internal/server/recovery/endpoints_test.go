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

// A running RunPod pod as its list reports it (GET /v2/pods, 2026-10-26):
// no runtime and no direct SSH, only the interactive proxy, whatever the
// pod's ports are. The single-pod read carries the direct endpoint.
func listedRunpodPod() provider.Instance {
	return provider.Instance{ExternalID: "pod-1", Power: provider.PowerRunning, SSHConnections: []provider.SSHConnection{
		{ID: "proxy", Type: "proxy", Host: "ssh.runpod.io", Port: 22, User: "pod-1-64410f3e", InstallSupported: false},
	}}
}

func describedRunpodPod() provider.Instance {
	pod := renumberedPod()
	pod.SSHConnections = append(pod.SSHConnections, provider.SSHConnection{ID: "proxy", Type: "proxy", Host: "ssh.runpod.io", Port: 22, User: "pod-1-64410f3e", InstallSupported: false})
	return pod
}

// The pod list says nothing of SSH, so the renumbered endpoint comes from the
// pod's own description; without it recovery kept probing the old port, where
// the restarted pod answers nothing, and never reinstalled the agent.
func TestRecoveryReadsTheRenumberedSSHEndpointFromTheInstanceDetail(t *testing.T) {
	r, st, _, fake := newRecoveryHarness(t)
	detail := describedRunpodPod()
	providers := &fakeProviders{instances: []provider.Instance{listedRunpodPod()}, detail: &detail}
	r.providers = providers
	fake.probeResult = provision.ProbeResult{Reachable: true, AgentInstalled: false}
	node := serverDialNode(t, st)
	if _, err := r.recoverNode(context.Background(), node, false); err != nil {
		t.Fatal(err)
	}
	if fake.probes.Load() != 1 || fake.probeReq.Port != 15755 || fake.probeReq.Host != "203.0.113.158" {
		t.Fatalf("probes = %d at %s:%d, want one at 203.0.113.158:15755", fake.probes.Load(), fake.probeReq.Host, fake.probeReq.Port)
	}
	credential, _, err := st.GetNodeSSHCredential(node.UUID)
	if err != nil || credential.Port != 15755 || credential.User != "root" {
		t.Fatalf("SSH credential = %+v, %v", credential, err)
	}
}

// The listing alone cannot move the credential to the proxy, and a pod that
// is restarting (no direct endpoint in its description yet) leaves it as it
// was; so does a detail endpoint that cannot be read.
func TestRecoveryKeepsTheSSHEndpointWithoutAnInstallableOneInTheDetail(t *testing.T) {
	restarting := listedRunpodPod()
	for name, providers := range map[string]*fakeProviders{
		"restarting pod": {instances: []provider.Instance{listedRunpodPod()}, detail: &restarting},
		"detail error":   {instances: []provider.Instance{listedRunpodPod()}, detailErr: errors.New("api down")},
		"no detail":      {instances: []provider.Instance{listedRunpodPod()}},
	} {
		t.Run(name, func(t *testing.T) {
			r, st, _, fake := newRecoveryHarness(t)
			r.providers = providers
			fake.probeResult = provision.ProbeResult{Reachable: false}
			node := serverDialNode(t, st)
			if _, err := r.recoverNode(context.Background(), node, false); err != nil {
				t.Fatal(err)
			}
			if fake.probes.Load() != 1 || fake.probeReq.Port != 10033 {
				t.Fatalf("probes = %d at port %d, want one at the stored port 10033", fake.probes.Load(), fake.probeReq.Port)
			}
			if providers.details != 1 {
				t.Fatalf("instance detail read %d times", providers.details)
			}
		})
	}
}
