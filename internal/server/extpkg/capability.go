package extpkg

import "maps"

// offered are the capabilities this server offers a package, by name, at
// their highest version: what a manifest's requires is checked against. Each
// part of the plugin host lists its capabilities here when it lands; within
// protocol major 1 a capability, once offered, is never removed and its
// version never goes down (docs/reference/plugin-protocol.md, "版本与兼容").
var offered = map[string]int{}

// Capabilities returns the capabilities this server offers.
func Capabilities() map[string]int { return maps.Clone(offered) }
