package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// A database without the row, or with a corrupt one, is the default branding;
// a written branding reads back unchanged and stores only the fields set.
func TestBrandingDefaultsAndRoundTrip(t *testing.T) {
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if got := st.Branding(); got != (Branding{}) {
		t.Fatalf("fresh branding = %+v, want zero", got)
	}
	want := Branding{ProductName: "Acme AI", Logo: "data:image/png;base64,AA=="}
	if err := st.SetBranding(want); err != nil {
		t.Fatal(err)
	}
	if raw := st.GetSetting(SettingBranding); raw != `{"product_name":"Acme AI","logo":"data:image/png;base64,AA=="}` {
		t.Fatalf("stored value = %s", raw)
	}
	if got := st.Branding(); got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if err := st.SetSetting(SettingBranding, `{"product_name":`); err != nil {
		t.Fatal(err)
	}
	if got := st.Branding(); got != (Branding{}) {
		t.Fatalf("corrupt value = %+v, want zero", got)
	}
}
