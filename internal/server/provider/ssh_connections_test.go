package provider

import "testing"

func TestSSHInstallCandidatesPreferDirectAndSkipTerminalOnly(t *testing.T) {
	inst := Instance{ExternalID: "i-1", SSHConnections: []SSHConnection{
		{ID: "terminal", Type: "proxy", Host: "terminal.example", Port: 22, InstallSupported: false},
		{ID: "proxy", Type: "proxy", Host: "proxy.example", Port: 2200, InstallSupported: true},
		{ID: "direct", Type: "direct", Host: "203.0.113.5", Port: 2222, InstallSupported: true},
	}}
	candidates, err := SSHInstallCandidates(inst, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].ID != "direct" || candidates[1].ID != "proxy" {
		t.Fatalf("candidates = %+v", candidates)
	}
}

func TestSSHInstallCandidatesExplicitSelectionDoesNotAddFallback(t *testing.T) {
	inst := Instance{SSHConnections: []SSHConnection{
		{ID: "direct", Type: "direct", Host: "203.0.113.5", Port: 22, InstallSupported: true},
		{ID: "proxy", Type: "proxy", Host: "proxy.example", Port: 2200, InstallSupported: true},
	}}
	candidates, err := SSHInstallCandidates(inst, "proxy")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != "proxy" {
		t.Fatalf("candidates = %+v", candidates)
	}
}

func TestSSHInstallCandidatesRejectTerminalOnlySelection(t *testing.T) {
	inst := Instance{SSHConnections: []SSHConnection{{ID: "terminal", Host: "terminal.example", Port: 22}}}
	if _, err := SSHInstallCandidates(inst, "terminal"); err == nil {
		t.Fatal("expected terminal-only connection to be rejected")
	}
}

// A provider command channel needs no host or credential, so it leads auto
// selection, and choosing it leaves the instance's SSH fields alone.
func TestCloudAssistantLeadsAutoSelectionAndIsNotAnSSHEndpoint(t *testing.T) {
	assistant := SSHConnection{ID: ConnectionCloudAssistant, Type: ConnectionCloudAssistant, InstallSupported: true}
	inst := Instance{ExternalID: "i-1", PublicIP: "203.0.113.5", SSHConnections: []SSHConnection{
		{ID: "direct", Type: "direct", Host: "203.0.113.5", Port: 22, User: "root", InstallSupported: true},
		assistant,
	}}
	candidates, err := SSHInstallCandidates(inst, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].ID != ConnectionCloudAssistant || candidates[1].ID != "direct" {
		t.Fatalf("candidates = %+v", candidates)
	}
	if applied := ApplySSHConnection(inst, assistant); applied.PublicIP != "203.0.113.5" || applied.SSHPort != 0 {
		t.Fatalf("applied = %+v", applied)
	}
	if got := SSHConnectionTarget(assistant); got != ConnectionCloudAssistant {
		t.Fatalf("target = %q", got)
	}
	// The legacy SSH fields still name the SSH endpoint.
	SetPreferredSSHConnection(&inst)
	if inst.SSHPort != 22 || inst.SSHUser != "root" {
		t.Fatalf("preferred SSH fields = %+v", inst)
	}
	if only, err := SSHInstallCandidates(Instance{SSHConnections: []SSHConnection{assistant}}, ConnectionCloudAssistant); err != nil || len(only) != 1 {
		t.Fatalf("explicit selection = %+v, %v", only, err)
	}
}
