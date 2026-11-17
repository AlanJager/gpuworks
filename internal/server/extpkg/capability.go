package extpkg

import (
	"maps"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// offered are the capabilities this server offers a package, by name, at
// their highest version: what a manifest's requires is checked against. Each
// part of the plugin host lists its capabilities here when it lands; within
// protocol major 1 a capability, once offered, is never removed and its
// version never goes down (docs/reference/plugin-protocol.md, "版本与兼容").
var offered = tableCapabilities()

// tableCapabilities offers every pushed table kind the core validates and
// enforces (package pushstate), at the newest schema pkg/extapi defines.
func tableCapabilities() map[string]int {
	out := map[string]int{}
	for _, kind := range extapi.TableKinds() {
		schemas := extapi.TableSchemas(kind)
		out[extapi.TableCapability(kind)] = schemas[len(schemas)-1]
	}
	return out
}

// Capabilities returns the capabilities this server offers.
func Capabilities() map[string]int { return maps.Clone(offered) }

// Offer is Capabilities as the handshake offers them: each capability at
// every version from 1 to its highest.
func Offer() map[string]extapi.Range {
	out := make(map[string]extapi.Range, len(offered))
	for name, v := range offered {
		out[name] = extapi.Range{Min: 1, Max: v}
	}
	return out
}
