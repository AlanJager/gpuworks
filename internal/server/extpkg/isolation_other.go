//go:build !linux

package extpkg

import "gpuworks.ai/gpuworks/pkg/extapi"

// HostIsolation is the strongest isolation this host can apply to a plugin
// process; only Linux has Landlock.
func HostIsolation() string { return extapi.IsolationNone }
