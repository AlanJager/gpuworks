//go:build cloudgpu

package l4

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/agentbin"
	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/testenv"
	"gpuworks.ai/gpuworks/internal/testenv/cloudprovider"
	"gpuworks.ai/gpuworks/internal/testenv/sshutil"
)

func verifyCloudServerForward(ctx context.Context, instance *cloudprovider.Instance, serverURL string) error {
	command := fmt.Sprintf("set -eu; export NO_PROXY=127.0.0.1,localhost; export no_proxy=127.0.0.1,localhost; curl -fsS %s/healthz >/dev/null; curl -fsS %s/install.sh | head -n 1 | grep -F '#!/bin/sh' >/dev/null", shellQuoteL4(serverURL), shellQuoteL4(serverURL))
	var lastOutput string
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		lastOutput, lastErr = runCloudCommand(ctx, instance, command)
		if lastErr == nil {
			return nil
		}
		if attempt == 4 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("remote health/install script request canceled after attempt %d: %w (last SSH error: %v): %s", attempt, ctx.Err(), lastErr, strings.TrimSpace(lastOutput))
		case <-time.After(time.Duration(attempt) * 5 * time.Second):
		}
	}
	return fmt.Errorf("remote health/install script request failed after 4 attempts: %w: %s", lastErr, strings.TrimSpace(lastOutput))
}

func hasProviderCredentials(p cloudprovider.ManagementProvider) bool {
	creds := p.ManagementCredentials()
	return strings.TrimSpace(creds.APIKey) != "" || strings.TrimSpace(creds.PublicKey) != "" || strings.TrimSpace(creds.PrivateKey) != ""
}

// installCloudAgentViaProviderAPI adds the provider, syncs it and installs
// the agent on instance through the provider API: dialing serverURL, or,
// with serverURL empty, dialed by the server at the port the driver chooses
// from the instance's mappings.
func installCloudAgentViaProviderAPI(ctx context.Context, t *testing.T, env *testenv.Env, p cloudprovider.CloudProvider, instance *cloudprovider.Instance, serverURL string) (string, error) {
	t.Helper()
	serverDial := serverURL == ""
	var expectedVastSSH *serverprovider.SSHConnection
	management := p.(cloudprovider.ManagementProvider)
	creds := management.ManagementCredentials()
	api := env.API(t)
	addBody, err := json.Marshal(map[string]any{
		"kind": p.Name(), "name": cloudprovider.TestNamePrefix + p.Name(), "endpoint": creds.Endpoint,
		"api_key": creds.APIKey, "public_key": creds.PublicKey, "private_key": creds.PrivateKey, "extra": creds.Extra,
	})
	if err != nil {
		return "", err
	}
	resp, err := api.PostJSON("/api/v1/providers", addBody)
	if err != nil {
		return "", err
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("add %s provider status=%d body=%s", p.Name(), resp.StatusCode, data)
	}
	var added struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(data, &added); err != nil || added.ID == 0 {
		return "", fmt.Errorf("decode added %s provider: id=%d err=%v", p.Name(), added.ID, err)
	}
	resp, err = api.PostJSON(fmt.Sprintf("/api/v1/providers/%d/sync", added.ID), []byte(`{}`))
	if err != nil {
		return "", err
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sync %s provider status=%d body=%s", p.Name(), resp.StatusCode, data)
	}
	if p.Name() == "vast" {
		var synced struct {
			Instances []serverprovider.Instance `json:"instances"`
		}
		if err := json.Unmarshal(data, &synced); err != nil {
			return "", fmt.Errorf("decode synced Vast instances: %w", err)
		}
		var found *serverprovider.Instance
		for index := range synced.Instances {
			if synced.Instances[index].ExternalID == instance.InstanceID {
				found = &synced.Instances[index]
				break
			}
		}
		if found == nil {
			return "", fmt.Errorf("synced Vast instance %s was not found", instance.InstanceID)
		}
		candidates, err := serverprovider.SSHInstallCandidates(*found, "auto")
		if err != nil || len(candidates) == 0 {
			return "", fmt.Errorf("synced Vast instance %s has no installable SSH connection: connections=%+v err=%v", instance.InstanceID, found.SSHConnections, err)
		}
		expected := candidates[0]
		if expected.Host != instance.PublicIP || expected.Port != instance.SSHPort {
			return "", fmt.Errorf("synced Vast preferred SSH connection=%+v, rented endpoint=%s:%d", expected, instance.PublicIP, instance.SSHPort)
		}
		expectedVastSSH = &expected
	}
	install := map[string]any{
		"external_id": instance.InstanceID, "server_url": serverURL,
		"ssh_key_pem": string(instance.PrivateKey), "password": instance.Password,
		"ssh_connection_id": "auto", "connect_mode": "agent_dial",
	}
	if serverDial {
		// Neither the listen port nor the address: the driver picks a mapped
		// port nothing on the instance holds.
		delete(install, "server_url")
		install["connect_mode"] = "server_dial"
	}
	installBody, err := json.Marshal(install)
	if err != nil {
		return "", err
	}
	resp, err = api.PostJSON(fmt.Sprintf("/api/v1/providers/%d/install", added.ID), installBody)
	if err != nil {
		return "", err
	}
	data, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("provider install status=%d body=%s", resp.StatusCode, data)
	}
	var result struct {
		NodeUUID      string                        `json:"node_uuid"`
		ServerURL     string                        `json:"server_url"`
		SSHConnection *serverprovider.SSHConnection `json:"ssh_connection"`
		SSHAttempts   []serverprovider.SSHAttempt   `json:"ssh_attempts"`
		Checks        []provision.CheckResult       `json:"checks"`
		Details       []provision.CheckResult       `json:"install_details"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	if strings.TrimRight(result.ServerURL, "/") != strings.TrimRight(serverURL, "/") {
		return "", fmt.Errorf("provider install server_url=%q, want %q", result.ServerURL, serverURL)
	}
	if result.NodeUUID == "" {
		return "", fmt.Errorf("provider install returned an empty node_uuid")
	}
	if p.Name() == "vast" {
		if result.SSHConnection == nil || expectedVastSSH == nil || result.SSHConnection.ID != expectedVastSSH.ID || result.SSHConnection.Type != expectedVastSSH.Type || result.SSHConnection.Host != expectedVastSSH.Host || result.SSHConnection.Port != expectedVastSSH.Port {
			return "", fmt.Errorf("Vast auto install selected SSH connection=%+v, want preferred %+v", result.SSHConnection, expectedVastSSH)
		}
		if len(result.SSHAttempts) != 1 || result.SSHAttempts[0].ConnectionID != result.SSHConnection.ID || result.SSHAttempts[0].Status != "selected" {
			return "", fmt.Errorf("Vast auto install SSH attempts=%+v", result.SSHAttempts)
		}
	}
	wantChecks := []string{"precheck", "install agent", "agent_dial"}
	if serverDial {
		wantChecks[2] = "server_dial"
	}
	if len(result.Checks) != len(wantChecks) {
		return "", fmt.Errorf("provider install checks=%+v, want %v", result.Checks, wantChecks)
	}
	var checkErr error
	for i, want := range wantChecks {
		t.Logf("provider install check: %s=%s %s", result.Checks[i].Name, result.Checks[i].Status, result.Checks[i].Detail)
		statusOK := result.Checks[i].Status == "ok"
		wantStatus := "ok"
		if want == "precheck" {
			// Provider prechecks are advisory. A VM without Docker is valid when
			// the installed agent selects udocker, which the suite verifies after
			// the node connects. Errors still indicate a missing hard prerequisite.
			statusOK = statusOK || result.Checks[i].Status == "warn"
			wantStatus = "ok/warn"
		} else if want == "agent_dial" && result.Checks[i].Status == "warn" && result.Checks[i].Detail == "agent started; waiting for its first session" {
			// The management API observes the first session for three seconds.
			// A cold VM may need longer; setupCloudNode still waits for this exact
			// node to become online, and the suite subsequently requires GPUs.
			statusOK = true
			wantStatus = "ok/warn(waiting for first session)"
		}
		if checkErr == nil && (result.Checks[i].Name != want || !statusOK) {
			checkErr = fmt.Errorf("provider install check %d=%+v, want %s=%s", i, result.Checks[i], want, wantStatus)
		}
	}
	if checkErr != nil {
		diagnostic, diagnosticErr := runCloudCommand(ctx, instance, `set +e
printf 'PATH=%s\n' "$PATH"
for executable in python3 python; do
  printf '%s=' "$executable"
  command -v "$executable" 2>&1 || true
  "$executable" --version 2>&1 || true
done
ls -l /usr/bin/python* /usr/local/bin/python* /root/miniconda3/bin/python* 2>&1 | head -40`)
		return "", fmt.Errorf("%w; ssh Python diagnostic (err=%v): %s", checkErr, diagnosticErr, strings.TrimSpace(diagnostic))
	}
	// The instance must now accept the server's platform key, however it got
	// there (provider API, or written after a password / harness-key login):
	// out-of-band recovery relies on nothing else.
	var platformKey *provision.CheckResult
	for i := range result.Details {
		t.Logf("provider install detail: %s=%s %s", result.Details[i].Name, result.Details[i].Status, result.Details[i].Detail)
		if result.Details[i].Name == "Platform key" {
			platformKey = &result.Details[i]
		}
	}
	if platformKey == nil || platformKey.Status != "ok" {
		return "", fmt.Errorf("provider install did not authorize the platform key: %+v", platformKey)
	}
	if serverDial {
		if err := assertCloudAgentServerDialed(ctx, env, instance, result.NodeUUID); err != nil {
			return "", err
		}
		return result.NodeUUID, nil
	}
	if err := assertCloudAgentServerURL(ctx, instance, serverURL); err != nil {
		return "", err
	}
	return result.NodeUUID, nil
}

// assertCloudAgentServerDialed checks a server-dialed agent: the node
// records the container port it listens on and the public address mapped to
// it, and the agent on the instance runs as `run --state` from a
// configuration that names no server and listens on that port.
func assertCloudAgentServerDialed(ctx context.Context, env *testenv.Env, instance *cloudprovider.Instance, nodeUUID string) error {
	node, err := env.Store().NodeByUUID(nodeUUID)
	if err != nil {
		return err
	}
	if node.ConnectMode != "server_dial" || node.ServerURL != "" || node.ServerDialListen == "" || node.ServerDialAddress == "" {
		return fmt.Errorf("server-dialed node mode=%q server_url=%q listen=%q address=%q", node.ConnectMode, node.ServerURL, node.ServerDialListen, node.ServerDialAddress)
	}
	command := "set -u\n" + agentconf.ShellFuncs + `
line=$({ ps axww -o args= 2>/dev/null || ps axww; } | grep -F -- 'gpuworks-agent run --state' | grep -v grep | head -n 1)
[ -n "$line" ] || { echo "no agent runs as run --state"; exit 1; }
printf 'mode=%s server=%s listen=%s\n' "$(gw_line_value "$line" CONNECT_MODE)" "$(gw_line_value "$line" SERVER)" "$(gw_line_value "$line" LISTEN)"`
	out, err := runCloudCommand(ctx, instance, command)
	if err != nil {
		return fmt.Errorf("read the server-dialed agent's configuration: %w: %s", err, out)
	}
	if want := "mode=server_dial server= listen=" + node.ServerDialListen; strings.TrimSpace(out) != want {
		return fmt.Errorf("server-dialed agent configuration = %q, want %q", strings.TrimSpace(out), want)
	}
	return nil
}

func topologyCloudTest(t *testing.T) bool {
	name := t.Name()
	return strings.Contains(name, "HAMi") || strings.Contains(name, "MultiNode") || strings.Contains(name, "GPUModelMatrix") || strings.Contains(name, "CompshareMultiGPU") || strings.Contains(name, "CompshareSuite")
}

func isCloudCapacityError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"no longer any instances available",
		"no preferred gpu candidate available",
		"requested capacity unavailable",
		// Gitee lists a model in /compute/specs but still rejects the create
		// with this message when every matching host is rented out.
		"没有找到符合条件的可用主机",
		"wait for runpod ssh endpoint",
		"wait for compshare ssh endpoint",
		"wait for ssh readiness",
		"claim install dir via sudo: eof",
		"ssh handshake: eof",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func waitCloudSSH(ctx context.Context, instance *cloudprovider.Instance) error {
	if instance.Password != "" {
		return sshutil.WaitSSHReadyPasswordAs(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password)
	}
	return sshutil.WaitSSHReadyAs(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey)
}

func openServerDialTunnel(ctx context.Context, instance *cloudprovider.Instance) (*sshutil.ServerDialTunnel, error) {
	const remoteAddr = "127.0.0.1:19090"
	if instance.Password != "" {
		return sshutil.NewServerDialTunnelPassword(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password, remoteAddr)
	}
	return sshutil.NewServerDialTunnel(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey, remoteAddr)
}

func openServerForward(ctx context.Context, instance *cloudprovider.Instance, localAddr string) (*sshutil.RemoteForward, error) {
	if instance.Password != "" {
		return sshutil.ForwardServerPassword(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.Password, localAddr)
	}
	return sshutil.ForwardServer(ctx, instance.PublicIP, instance.SSHPort, instance.Username, instance.PrivateKey, localAddr)
}

func installCloudAgent(ctx context.Context, instance *cloudprovider.Instance, serverAddr, token string) error {
	installer := provision.NewSSHInstaller(func(name string) ([]byte, error) {
		return agentbin.Read("", name)
	})
	req := provision.SSHInstallRequest{
		Target: instance.PublicIP, User: instance.Username, Port: instance.SSHPort,
		ServerURL: serverAddr, Token: token, ConnectMode: "server_dial",
		ServerDialListen: "127.0.0.1:19090",
		InstallDir:       instance.DataDiskPath,
		SSHAuth:          provision.SSHAuth{Password: instance.Password, PrivateKeyPEM: string(instance.PrivateKey)},
	}
	_, err := installer.InstallSSH(ctx, req)
	if err != nil {
		return err
	}
	return assertCloudAgentServerURL(ctx, instance, serverAddr)
}

// installCloudAgentViaAPI exercises the same management-plane one-click
// installer used by the UI. Keeping this path in acceptance tests catches
// regressions in token creation, privilege fallback, server_dial setup and
// agent binary selection instead of bypassing them with a test-only token.
func installCloudAgentViaAPI(ctx context.Context, t *testing.T, env *testenv.Env, instance *cloudprovider.Instance, providerKind, name, serverURL, connectMode, listen, serverDialAddress string) error {
	body, err := json.Marshal(map[string]any{
		"name": name, "provider_kind": providerKind, "region": instance.Region, "target": instance.PublicIP, "user": instance.Username, "port": instance.SSHPort,
		"password": instance.Password, "private_key_pem": string(instance.PrivateKey),
		"connect_mode": connectMode, "server_dial_listen": listen, "server_dial_address": serverDialAddress, "server_url": serverURL,
	})
	if err != nil {
		return err
	}
	api := env.API(t)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.URL()+"/api/v1/nodes/install", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		diagnostic, diagnosticErr := runCloudCommand(ctx, instance, "set +e; printf 'id='; id -u; printf '\\nuname='; uname -a 2>&1; printf '\\ndpkg='; dpkg --print-architecture 2>&1; printf '\\narch='; arch 2>&1; printf '\\ncpu='; head -n 3 /proc/cpuinfo 2>&1")
		if diagnosticErr != nil {
			diagnostic += " (diagnostic error: " + diagnosticErr.Error() + ")"
		}
		return fmt.Errorf("one-click agent install status=%d body=%s; ssh diagnostic=%s", resp.StatusCode, data, strings.TrimSpace(diagnostic))
	}
	var report provision.SSHInstallReport
	if err := json.Unmarshal(data, &report); err == nil {
		if strings.TrimRight(report.ServerURL, "/") != strings.TrimRight(serverURL, "/") {
			return fmt.Errorf("installer report server_url=%q, want %q", report.ServerURL, serverURL)
		}
		for _, check := range report.Checks {
			t.Logf("one-click install check: %s=%s %s", check.Name, check.Status, check.Detail)
		}
	}
	return assertCloudAgentServerURL(ctx, instance, serverURL)
}

// assertCloudAgentServerURL checks that the running agent dials serverURL,
// as its configuration file (or an earlier install's arguments) says.
func assertCloudAgentServerURL(ctx context.Context, instance *cloudprovider.Instance, serverURL string) error {
	needle := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	command := "set -u\nwant=" + shellQuoteL4(needle) + "\n" + agentconf.ShellFuncs + `
if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet gpuworks-agent 2>/dev/null; then
  srv=$(gw_unit_value SERVER)
  [ "${srv%/}" = "$want" ] && exit 0
fi
{ ps axww -o args= 2>/dev/null || ps axww; } | grep -F -- gpuworks-agent | grep -v grep | while IFS= read -r line; do
  srv=$(gw_line_value "$line" SERVER)
  [ "${srv%/}" = "$want" ] && echo found
done | grep -q found`
	if _, err := runCloudCommand(ctx, instance, command); err != nil {
		return fmt.Errorf("the running agent does not dial server_url %q: %w", needle, err)
	}
	return nil
}

func shellQuoteL4(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func dialCloudAgent(ctx context.Context, env *testenv.Env, addr, token string) error {
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		lastErr = env.DialServerAgent(attemptCtx, addr, token)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("dial cloud agent through SSH: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(3 * time.Second):
		}
	}
}
