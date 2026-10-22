package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// A database without the row, or with a corrupt one, is the restrictive
// policy; a written policy reads back unchanged.
func TestSecurityPolicyDefaultsAndRoundTrip(t *testing.T) {
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if got := st.SecurityPolicy(); got != (SecurityPolicy{}) {
		t.Fatalf("fresh policy = %+v, want zero", got)
	}
	if err := st.SetSecurityPolicy(SecurityPolicy{PlaygroundPreviewNetwork: true}); err != nil {
		t.Fatal(err)
	}
	if raw := st.GetSetting(SettingSecurityPolicy); raw != `{"playground_preview_network":true}` {
		t.Fatalf("stored value = %s", raw)
	}
	if got := st.SecurityPolicy(); !got.PlaygroundPreviewNetwork {
		t.Fatalf("round trip = %+v", got)
	}
	if err := st.SetSetting(SettingSecurityPolicy, `{"playground_preview_network":`); err != nil {
		t.Fatal(err)
	}
	if got := st.SecurityPolicy(); got != (SecurityPolicy{}) {
		t.Fatalf("corrupt value = %+v, want zero", got)
	}
}
