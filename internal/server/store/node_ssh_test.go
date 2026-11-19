package store

import (
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestNodeSSHCredentialEncryptedRoundTripAndRecoveryQueries(t *testing.T) {
	st, err := Open("file:node_ssh_roundtrip?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := auth.NewSecretbox("node-ssh-test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	node, _, err := st.CreateNodePending("ssh-node", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, NodeStateOffline); err != nil {
		t.Fatal(err)
	}

	want := NodeSSHSecrets{Password: "high entropy password", PrivateKeyPEM: "-----BEGIN PRIVATE KEY-----\nsecret-key\n-----END PRIVATE KEY-----"}
	row, err := st.SetNodeSSHCredential(node.UUID, "gpu.example", 2222, "ubuntu", want)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.CredsCipher, want.Password) || strings.Contains(row.CredsCipher, "secret-key") {
		t.Fatalf("credential stored in plaintext: %q", row.CredsCipher)
	}
	gotRow, got, err := st.GetNodeSSHCredential(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRow.Host != "gpu.example" || gotRow.Port != 2222 || gotRow.User != "ubuntu" || got != want {
		t.Fatalf("round trip row=%+v secrets=%+v", gotRow, got)
	}
	loaded, err := st.NodeByUUID(node.UUID)
	if err != nil || !loaded.HasSSHCredential {
		t.Fatalf("credential flag node=%+v err=%v", loaded, err)
	}
	needed, err := st.NodesNeedingRecovery()
	if err != nil || len(needed) != 1 || needed[0].UUID != node.UUID {
		t.Fatalf("needed=%+v err=%v", needed, err)
	}

	if err := st.SetNodeRecoveryPhase(node.UUID, RecoveryPhaseStopped, 7, timePtrStore(time.Now()), RecoveryError{Message: "destroyed"}); err != nil {
		t.Fatal(err)
	}
	needed, err = st.NodesNeedingRecovery()
	if err != nil || len(needed) != 0 {
		t.Fatalf("stopped needed=%+v err=%v", needed, err)
	}
	if err := st.SetNodeRecoveryPhase(node.UUID, RecoveryPhaseBlocked, 2, timePtrStore(time.Now()), RecoveryError{Message: "uploading: upload_stalled"}); err != nil {
		t.Fatal(err)
	}
	needed, err = st.NodesNeedingRecovery()
	if err != nil || len(needed) != 0 {
		t.Fatalf("blocked needed=%+v err=%v", needed, err)
	}
	if err := st.DeleteNodeSSHCredential(node.UUID); err != nil {
		t.Fatal(err)
	}
	loaded, _ = st.NodeByUUID(node.UUID)
	if loaded.HasSSHCredential {
		t.Fatal("credential flag remained after delete")
	}
}

func TestMigration51AddsOutOfBandRecoverySchema(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	version, err := st.CurrentMigration()
	if err != nil {
		t.Fatal(err)
	}
	if version < 51 {
		t.Fatalf("migration version=%d", version)
	}
	if !st.DB().Migrator().HasTable(&NodeSSHCredential{}) {
		t.Fatal("node_ssh_credential table missing")
	}
	for _, column := range []string{"RecoveryPhase", "RecoveryAttempts", "LastProbeAt", "LastRecoveryError", "ServerDialListen"} {
		if !st.DB().Migrator().HasColumn(&Node{}, column) {
			t.Fatalf("nodes.%s missing", column)
		}
	}
	if st.DB().Migrator().HasColumn("gpus", "partition_mode") {
		t.Fatal("legacy gpus.partition_mode survived partition capability migration")
	}
}

func TestNodeRecoveryTransitionsPublishTimelineEvents(t *testing.T) {
	b := bus.New()
	st, err := Open("file:node_recovery_events?mode=memory&cache=shared", b)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, _, err := st.CreateNodePending("recovery-events", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	sub := b.Subscribe(12, "node")
	defer sub.Close()
	probeAt := time.Now()
	phases := []string{RecoveryPhaseProbing, RecoveryPhaseArtifactCheck, RecoveryPhaseUploading, RecoveryPhaseVerifying, RecoveryPhaseReplacing, RecoveryPhaseRestarting, RecoveryPhaseReconnecting, RecoveryPhaseBlocked}
	for _, phase := range phases {
		if err := st.SetNodeRecoveryPhase(node.UUID, phase, 2, &probeAt, RecoveryError{Message: "checking"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ClearNodeRecovery(node.UUID); err != nil {
		t.Fatal(err)
	}
	wants := make([]string, 0, len(phases)+1)
	for _, phase := range phases {
		wants = append(wants, "recovery:"+phase)
	}
	wants = append(wants, "recovery:cleared")
	for _, want := range wants {
		select {
		case event := <-sub.C():
			if event.Topic != "node" || event.Key != node.UUID || event.Reason != want {
				t.Fatalf("event=%+v, want reason %q", event, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %q event", want)
		}
	}
}

func timePtrStore(value time.Time) *time.Time { return &value }

// An endpoint without a password or key is valid: the server then logs in
// with the default keys in its own ~/.ssh, and recovery must still find it.
func TestNodeSSHCredentialWithoutSecretsUsesServerDefaultKeys(t *testing.T) {
	st, err := Open("file:node_ssh_default_keys?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := auth.NewSecretbox("node-ssh-test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	node, _, err := st.CreateNodePending("ssh-default-key", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeSSHCredential(node.UUID, "gpu.example", 51223, "root", NodeSSHSecrets{}); err != nil {
		t.Fatal(err)
	}
	row, secrets, err := st.GetNodeSSHCredential(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Host != "gpu.example" || row.Port != 51223 || row.User != "root" || !secrets.Empty() {
		t.Fatalf("row=%+v secrets=%+v", row, secrets)
	}
	loaded, err := st.NodeByUUID(node.UUID)
	if err != nil || !loaded.HasSSHCredential {
		t.Fatalf("credential flag node=%+v err=%v", loaded, err)
	}
}

// Hosts behind one jump gateway share its address; the forwarded port is what
// tells them apart, and a missing port means OpenSSH's 22.
func TestNodesWithSSHEndpointMatchesHostAndPort(t *testing.T) {
	st, err := Open("file:node_ssh_endpoint_match?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := auth.NewSecretbox("node-ssh-test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	for _, c := range []struct {
		name string
		port int
	}{{"gw-a", 39911}, {"gw-b", 39912}, {"plain", 22}} {
		node, _, err := st.CreateNodePending(c.name, "agent_dial")
		if err != nil {
			t.Fatal(err)
		}
		host := "gateway.example"
		if c.name == "plain" {
			host = "plain.example"
		}
		if _, err := st.SetNodeSSHCredential(node.UUID, host, c.port, "sparker", NodeSSHSecrets{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		host string
		port int
		want string
	}{
		{"gateway.example", 39911, "gw-a"},
		{"gateway.example", 39912, "gw-b"},
		{"gateway.example", 39913, ""},
		{"gateway.example", 22, ""},
		{"plain.example", 0, "plain"},
		{" [plain.example] ", 22, "plain"},
		{"plain.example", 2222, ""},
		{"", 22, ""},
	} {
		nodes, err := st.NodesWithSSHEndpoint(tc.host, tc.port)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, n := range nodes {
			names = append(names, n.Name)
		}
		if got := strings.Join(names, ","); got != tc.want {
			t.Errorf("NodesWithSSHEndpoint(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
		}
	}
}
