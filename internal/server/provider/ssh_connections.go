package provider

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/pkg/localization"
)

// ConnectionCloudAssistant is a provider's own command channel into the
// instance (Alibaba Cloud's Cloud Assistant): no SSH, host or credential, so
// an instance without a public address can still be installed. The driver
// that lists it runs the install through it.
const ConnectionCloudAssistant = "cloud_assistant"

// ServerDialCandidates drops provider command channels from an automatic
// choice for an agent the server dials: a command channel installs by having
// the instance download the agent from the server, which such an instance
// cannot reach. Trying one would only add a failed attempt to the report.
// An explicit choice is kept, and its driver says why it cannot.
func ServerDialCandidates(candidates []SSHConnection, mode, selectedID string) []SSHConnection {
	if mode != "server_dial" || (strings.TrimSpace(selectedID) != "" && strings.TrimSpace(selectedID) != "auto") {
		return candidates
	}
	out := make([]SSHConnection, 0, len(candidates))
	for _, candidate := range candidates {
		if IsSSHConnection(candidate) {
			out = append(out, candidate)
		}
	}
	if len(out) == 0 {
		return candidates
	}
	return out
}

// IsSSHConnection reports whether the connection is an SSH endpoint, as
// opposed to a provider command channel.
func IsSSHConnection(connection SSHConnection) bool {
	return connection.Type != ConnectionCloudAssistant
}

// SetPreferredSSHConnection keeps the legacy endpoint fields populated for
// one compatibility release while ssh_connections is adopted by clients.
func SetPreferredSSHConnection(inst *Instance) {
	connections, err := SSHInstallCandidates(*inst, "auto")
	if err != nil || len(connections) == 0 {
		return
	}
	var preferred SSHConnection
	for _, connection := range connections {
		if IsSSHConnection(connection) {
			preferred = connection
			break
		}
	}
	if preferred.ID == "" {
		return
	}
	inst.SSHPort = preferred.Port
	inst.SSHUser = preferred.User
	if strings.TrimSpace(inst.PublicIP) == "" {
		inst.PublicIP = preferred.Host
	}
}

// SSHInstallCandidates returns programmable SSH paths in fallback order.
func SSHInstallCandidates(inst Instance, selectedID string) ([]SSHConnection, error) {
	selectedID = strings.TrimSpace(selectedID)
	if selectedID == "" {
		selectedID = "auto"
	}
	connections := append([]SSHConnection(nil), inst.SSHConnections...)
	if len(connections) == 0 && strings.TrimSpace(inst.PublicIP) != "" {
		port := inst.SSHPort
		if port == 0 {
			port = 22
		}
		connections = []SSHConnection{{
			ID: "legacy", Type: "direct", Label: localization.SameText("SSH", requiredProviderLocales...), Host: inst.PublicIP,
			Port: port, User: sshFirstNonEmpty(inst.SSHUser, "root"), InstallSupported: true,
		}}
	}
	if selectedID != "auto" {
		for _, connection := range connections {
			if connection.ID != selectedID {
				continue
			}
			if !connection.InstallSupported {
				return nil, fmt.Errorf("SSH connection %q does not support one-click installation", selectedID)
			}
			if err := validateSSHConnection(connection); err != nil {
				return nil, err
			}
			return []SSHConnection{connection}, nil
		}
		return nil, fmt.Errorf("SSH connection %q was not found", selectedID)
	}
	eligible := make([]SSHConnection, 0, len(connections))
	for _, connection := range connections {
		if !connection.InstallSupported || validateSSHConnection(connection) != nil {
			continue
		}
		eligible = append(eligible, connection)
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		return sshConnectionPriority(eligible[i]) < sshConnectionPriority(eligible[j])
	})
	if len(eligible) == 0 {
		return nil, fmt.Errorf("instance %q has no SSH connection that supports one-click installation", inst.ExternalID)
	}
	return eligible, nil
}

func ApplySSHConnection(inst Instance, connection SSHConnection) Instance {
	if !IsSSHConnection(connection) {
		return inst
	}
	inst.PublicIP = connection.Host
	inst.SSHPort = connection.Port
	inst.SSHUser = sshFirstNonEmpty(connection.User, "root")
	return inst
}

func SSHConnectionTarget(connection SSHConnection) string {
	if !IsSSHConnection(connection) {
		return connection.Type
	}
	return sshFirstNonEmpty(connection.User, "root") + "@" + net.JoinHostPort(connection.Host, strconv.Itoa(connection.Port))
}

func validateSSHConnection(connection SSHConnection) error {
	if !IsSSHConnection(connection) {
		if strings.TrimSpace(connection.ID) == "" {
			return fmt.Errorf("connection of type %q has no id", connection.Type)
		}
		return nil
	}
	if strings.TrimSpace(connection.ID) == "" || strings.TrimSpace(connection.Host) == "" || connection.Port <= 0 || connection.Port > 65535 {
		return fmt.Errorf("SSH connection %q has an invalid host or port", connection.ID)
	}
	return nil
}

func sshConnectionPriority(connection SSHConnection) int {
	switch strings.ToLower(strings.TrimSpace(connection.Type)) {
	case ConnectionCloudAssistant:
		// Needs no credential and no public address.
		return -1
	case "direct":
		return 0
	case "proxy":
		return 1
	default:
		return 2
	}
}

func sshFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
