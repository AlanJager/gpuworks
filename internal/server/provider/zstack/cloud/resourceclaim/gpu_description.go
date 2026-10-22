package resourceclaim

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

func (s *Service) gpuOwnershipDescription(nodeUUID string) (string, error) {
	node, err := s.store.NodeByUUID(nodeUUID)
	if err != nil {
		return "", err
	}
	address, err := url.Parse(node.ServerURL)
	if err != nil || address.Hostname() == "" {
		return "", fmt.Errorf("Cloud node has no valid GPUWorks server address")
	}
	return "被gpuworks-" + address.Hostname() + "使用中", nil
}

func hasGPUOwnershipDescription(description, ownership string) bool {
	for _, line := range strings.Split(description, "\n") {
		if line == ownership {
			return true
		}
	}
	return false
}

func addGPUOwnershipDescription(description, ownership string) (string, error) {
	if !hasGPUOwnershipDescription(description, ownership) {
		if description != "" {
			description += "\n"
		}
		description += ownership
	}
	// APIUpdatePciDeviceMsg limits description to 2048 characters.
	if utf8.RuneCountInString(description) > 2048 {
		return "", fmt.Errorf("Cloud GPU description exceeds 2048 characters")
	}
	return description, nil
}

func removeGPUOwnershipDescription(description, ownership string) string {
	lines := strings.Split(description, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if line != ownership {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
