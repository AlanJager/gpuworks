package featuregate

import (
	"testing"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Plugins declare feature keys and push availability in the wire formats of
// pkg/extapi, which cannot import this package. The two must agree on the
// key grammar and the access names, or a key a plugin validly declares
// would be one the gate refuses.
func TestWireFormatAgreesWithTheGate(t *testing.T) {
	for _, k := range []string{
		"audit", "iam.custom_roles", "acme.backup.restore", "ab", "a", "acme.x", "Acme", "acme..x",
		"acme.backup.restore.all", "1acme", "acme_", "acme-x", "acme.b2", "acme." + string(make([]byte, 40)),
		"abcdefghijklmnopqrstuvwxyzabcdef", "abcdefghijklmnopqrstuvwxyzabcdefg",
	} {
		if got, want := extapi.ValidFeatureKey(k), Key(k).Valid(); got != want {
			t.Errorf("key %q: wire format says %v, the gate %v", k, got, want)
		}
	}
	for access, wire := range map[Access]string{
		Full: extapi.AccessFull, ReadOnly: extapi.AccessReadOnly, Locked: extapi.AccessLocked, Hidden: extapi.AccessHidden,
	} {
		if access.String() != wire {
			t.Errorf("access %d: gate says %q, wire format %q", access, access.String(), wire)
		}
	}
}
