package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
)

// InboundRuleChecker is an optional driver capability: read, changing
// nothing, whether the provider's firewall (Alibaba Cloud security groups)
// lets the server reach a server-dialed agent's TCP port. The precheck adds
// the row it returns.
type InboundRuleChecker interface {
	CheckInbound(ctx context.Context, creds Credentials, inst Instance, port int) InstallCheck
}

// ServerDialPortMapper is an optional driver capability for providers that
// reach a container only through ports they map to a public address (Vast,
// RunPod). The server cannot know beforehand where such an agent listens or
// where it is reached: the driver chooses a mapped port nothing on the
// instance holds when it installs (InstallReport.ServerDialListen and
// ServerDialAddress), and ServerDialAddressFor says where that port is
// reached now — a mapping a migration or a configuration change renumbered.
type ServerDialPortMapper interface {
	ServerDialAddressFor(ctx context.Context, creds Credentials, externalID, listen string) (string, error)
}

// ErrInstanceRestartRequired is a driver's answer when the change an install
// needs (a port for a server-dialed agent, the platform key on the instance)
// restarts the instance, and the operator has not agreed to that
// (InstallRequest.AllowInstanceRestart).
var ErrInstanceRestartRequired = errors.New("the change the install needs restarts the instance")

// InstanceRestartError says why the restart is needed; it matches
// ErrInstanceRestartRequired. Need names what the restart is for (the
// console words its question by it): "port", "ssh_key" or "ssh_key_port".
type InstanceRestartError struct {
	Reason string
	Need   string
}

func (e *InstanceRestartError) Error() string {
	return ErrInstanceRestartRequired.Error() + ": " + e.Reason
}

func (e *InstanceRestartError) Is(target error) bool { return target == ErrInstanceRestartRequired }

// ChooseMappedPort picks the container port a server-dialed agent listens on
// from a provider's TCP mappings (container port → public port): the one
// the operator asked for when it is mapped and free, else the lowest mapped
// port nothing listens on. SSH's port is never given away.
func ChooseMappedPort(mapped map[int]int, listening map[int]bool, preferred int) (int, bool) {
	free := func(port int) bool {
		_, ok := mapped[port]
		return ok && port != 22 && !listening[port]
	}
	if preferred > 0 && free(preferred) {
		return preferred, true
	}
	ports := make([]int, 0, len(mapped))
	for port := range mapped {
		if free(port) {
			ports = append(ports, port)
		}
	}
	if len(ports) == 0 {
		return 0, false
	}
	sort.Ints(ports)
	return ports[0], true
}

// ListenOnPort is the listen address for a chosen container port.
func ListenOnPort(port int) string { return "0.0.0.0:" + strconv.Itoa(port) }

// PortOf is the port of a listen address, 0 when it has none.
func PortOf(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}

// ServerDialAddress is where the provider maps listen's port on the
// instance now (ServerDialPortMapper); ErrUnsupported for a provider that
// does not map ports, where the address the operator gave stays.
func (s *Service) ServerDialAddress(ctx context.Context, providerID uint, externalID, listen string) (string, error) {
	p, err := s.store.GetProvider(providerID)
	if err != nil {
		return "", err
	}
	drv, ok := s.driver(p.Kind)
	if !ok {
		return "", fmt.Errorf("driver %s not registered", p.Kind)
	}
	mapper, ok := drv.(ServerDialPortMapper)
	if !ok {
		return "", ErrUnsupported
	}
	creds, err := s.creds(p)
	if err != nil {
		return "", err
	}
	return mapper.ServerDialAddressFor(ctx, creds, externalID, listen)
}
