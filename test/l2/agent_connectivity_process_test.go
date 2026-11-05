//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Exercise the actual binaries and RPC wire: a node without SSH can report its
// live connection, and a separate local diagnostic cannot replace that link.
func TestAgentConnectivityDiagnosticsWithoutSSHAcrossProcesses(t *testing.T) {
	h := newStreamHarness(t)
	response, raw := authedDo(t, http.MethodGet, h.base, h.adminToken, "/api/v1/nodes/"+h.node.UUID, nil)
	var node map[string]json.RawMessage
	if response.StatusCode != http.StatusOK || json.Unmarshal(raw, &node) != nil || string(node["has_ssh_credential"]) != "false" {
		t.Fatal("fixture must be an actual online node without saved SSH credentials")
	}
	type check struct {
		Stage   string `json:"stage"`
		Outcome string `json:"outcome"`
	}
	type report struct {
		AgentReport *proto.DoctorReport `json:"agent_report"`
		Checks      []check             `json:"checks"`
	}
	diagnose := func() report {
		t.Helper()
		response, raw := authedDo(t, http.MethodPost, h.base, h.adminToken, "/api/v1/nodes/"+h.node.UUID+"/connection-diagnostics", []byte(`{}`))
		var result report
		if response.StatusCode != http.StatusOK || json.Unmarshal(raw, &result) != nil {
			t.Fatalf("connection diagnostics HTTP status=%d", response.StatusCode)
		}
		if bytes.Contains(raw, []byte(h.nodeToken)) {
			t.Fatal("connection report disclosed its node token")
		}
		return result
	}
	assertLive := func() {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			result := diagnose()
			if result.AgentReport != nil && proto.ConnectionValue(*result.AgentReport, "connection.source") == "agent_runtime" && proto.ConnectionValue(*result.AgentReport, "connection.control") == "connected" && proto.ConnectionValue(*result.AgentReport, "connection.data_lane") == "attached" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("real no-SSH agent failed to report its current control and data connection")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	assertLive()

	configure := exec.Command(h.agentBin, "configure", "--state", h.stateDir, "--server", h.base)
	configure.Stdin = strings.NewReader(h.nodeToken + "\n")
	if err := configure.Run(); err != nil {
		t.Fatalf("configure local diagnostic identity: %v", err)
	}
	cli := exec.Command(h.agentBin, "doctor", "--connectivity", "--state", h.stateDir, "--json")
	// This test observes the file configuration, independent of the test runner's
	// own launch overrides. The running agent still uses its original flags.
	cli.Env = append(os.Environ(), "GPUWORKS_SERVER=", "GPUWORKS_TOKEN=", "GPUWORKS_CONNECT_MODE=")
	var stderr bytes.Buffer
	cli.Stderr = &stderr
	stdout, err := cli.Output()
	if err != nil {
		t.Fatalf("local connectivity CLI failed: %v", err)
	}
	var local proto.DoctorReport
	if json.Unmarshal(stdout, &local) != nil || proto.ConnectionValue(local, "connection.source") != "cli" || proto.ConnectionValue(local, "connection.server_source") != "state_file" || proto.ConnectionValue(local, "connection.control") != "smoke_verified" || proto.ConnectionValue(local, "connection.data_lane") != "unverified" {
		t.Fatal("CLI must emit standalone JSON with its own file/Smoke scope")
	}
	if bytes.Contains(stdout, []byte(h.nodeToken)) || bytes.Contains(stderr.Bytes(), []byte(h.nodeToken)) {
		t.Fatal("CLI disclosed its node token")
	}
	assertLive()

	stopProcess(h.agent)
	h.agent = nil
	waitNode(t, h.base, h.adminToken, func(node processNode) bool { return node.UUID == h.node.UUID && node.State == "offline" })
	offline := diagnose()
	if offline.AgentReport != nil {
		t.Fatal("offline node must not return a fresh runtime report")
	}
	for _, check := range offline.Checks {
		if check.Stage == "agent_control" && check.Outcome == "offline" {
			return
		}
	}
	t.Fatal("offline node must retain an explicit partial diagnostic outcome")
}
