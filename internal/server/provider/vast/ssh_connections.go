package vast

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func (in *instance) toInstance() provider.Instance {
	inst := provider.Instance{
		ExternalID: in.ID.String(), Name: in.name(), GPUModel: in.GPUName, GPUCount: in.NumGPUs,
		Power: in.power(), PricePerHr: in.DPHTotal, Region: in.Geolocation, PublicIP: in.PublicIPAddr, Kind: "container",
	}
	seen := map[string]bool{}
	for _, port := range in.directSSHPorts() {
		key := net.JoinHostPort(in.PublicIPAddr, strconv.Itoa(port))
		if in.PublicIPAddr == "" || seen[key] {
			continue
		}
		seen[key] = true
		id := "direct"
		if len(inst.SSHConnections) > 0 {
			id = fmt.Sprintf("direct-%d", len(inst.SSHConnections)+1)
		}
		inst.SSHConnections = append(inst.SSHConnections, provider.SSHConnection{
			ID: id, Type: "direct", Label: provider.SSHConnectionLabel("direct"), Host: in.PublicIPAddr, Port: port, User: "root", InstallSupported: true,
		})
	}
	if in.SSHHost != "" && in.SSHPort > 0 {
		inst.SSHConnections = append(inst.SSHConnections, provider.SSHConnection{
			ID: "proxy", Type: "proxy", Label: provider.SSHConnectionLabel("proxy"), Host: in.SSHHost, Port: in.SSHPort, User: "root", InstallSupported: true,
		})
	}
	provider.SetPreferredSSHConnection(&inst)
	return inst
}

func (in *instance) directSSHPorts() []int {
	if len(in.Ports) == 0 {
		return nil
	}
	var mappings map[string][]struct {
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal(in.Ports, &mappings); err != nil {
		return nil
	}
	ports := make([]int, 0, len(mappings["22/tcp"]))
	for _, mapping := range mappings["22/tcp"] {
		port, err := strconv.Atoi(mapping.HostPort)
		if err == nil && port > 0 && port <= 65535 {
			ports = append(ports, port)
		}
	}
	return ports
}
