// Package nodeenv identifies provider-managed environments from observed host facts.
package nodeenv

import "strings"

func IsAutoDLContainer(hostname string) bool {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	return strings.HasPrefix(hostname, "autodl-container-") && len(hostname) > len("autodl-container-")
}
