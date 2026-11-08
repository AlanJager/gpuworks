package proto

import "strings"

// EnginePortRefused prefixes the failure an agent reports when a host port
// the server allocated was already taken as the launch began. The agent
// checks the ports before it pulls an image or creates a container, so such
// an attempt started nothing: the server retries it at once with other ports
// and does not charge it to the retry budget (docs/adr/0029).
const EnginePortRefused = "ENGINE_PORT_REFUSED"

// EnginePortInUse prefixes the failure of an engine whose API port another
// engine answered on after the container started. That attempt did start a
// container and counts as any failed attempt does.
const EnginePortInUse = "ENGINE_PORT_IN_USE"

// IsEnginePortRefusal says an instance's failure message is a pre-create port
// refusal (EnginePortRefused).
func IsEnginePortRefusal(message string) bool {
	return strings.HasPrefix(message, EnginePortRefused+":")
}
