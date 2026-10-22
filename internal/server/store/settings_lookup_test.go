package store

import (
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestLookupSettingDistinguishesMissingAndStorageFailure(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "settings.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if got, err := st.LookupSetting(SettingNetwork); err != nil || got != "" {
		t.Fatalf("absent=%q err=%v", got, err)
	}
	const configured = `{"http_proxy":"http://proxy.example:3128"}`
	if err := st.SetSetting(SettingNetwork, configured); err != nil {
		t.Fatal(err)
	}
	if got, err := st.LookupSetting(SettingNetwork); err != nil || got != configured {
		t.Fatalf("configured=%q err=%v", got, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupSetting(SettingNetwork); err == nil {
		t.Fatal("storage failure reported as absent setting")
	}
}
