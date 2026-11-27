package app

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// The API's session policy carries auth.local_password from the
// configuration; if the field were dropped here the policy would quietly
// fall back to letting everyone sign in with a password.
func TestSessionPolicyCarriesTheLocalPasswordPolicy(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AuthLocalPassword = serverconfig.LocalPasswordLocalAdmin
	if got := cfg.sessionPolicy(nil).LocalPassword; got != auth.LocalPasswordLocalAdmin {
		t.Fatalf("session policy LocalPassword = %q, want %q", got, auth.LocalPasswordLocalAdmin)
	}
	cfg.AuthLocalPassword = ""
	if got := cfg.sessionPolicy(nil).Normalized().LocalPassword; got != auth.LocalPasswordAll {
		t.Fatalf("default session policy LocalPassword = %q, want %q", got, auth.LocalPasswordAll)
	}
}

// server.yaml and the session policy name the same two values: the file is
// validated against one set and enforced against the other.
func TestLocalPasswordValuesAgreeBetweenTheFileAndThePolicy(t *testing.T) {
	if serverconfig.LocalPasswordAll != auth.LocalPasswordAll || serverconfig.LocalPasswordLocalAdmin != auth.LocalPasswordLocalAdmin {
		t.Fatalf("serverconfig (%q, %q) and auth (%q, %q) disagree",
			serverconfig.LocalPasswordAll, serverconfig.LocalPasswordLocalAdmin, auth.LocalPasswordAll, auth.LocalPasswordLocalAdmin)
	}
	for _, v := range []string{auth.LocalPasswordAll, auth.LocalPasswordLocalAdmin} {
		if err := (serverconfig.Auth{LocalPassword: v}).Validate(); err != nil {
			t.Fatalf("the policy value %q is refused in server.yaml: %v", v, err)
		}
	}
}
