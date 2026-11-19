//go:build !linux && !darwin

package hostidentity

// local knows no facts on other systems.
func local() Facts { return Facts{V: FileVersion} }
