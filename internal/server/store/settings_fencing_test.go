package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Controller fencing facts share the settings table but are not public JSON
// documents: listing them made every full settings read fail.
func TestSchedulingFencingFactsAreNotPublicSettings(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	facts := map[string]string{"scheduling.generation": "7", "scheduling.owner": "0123456789abcdef0123456789abcdef", "scheduling.confirmation": "fedcba9876543210fedcba9876543210"}
	for key, value := range facts {
		if err := st.SetSetting(SettingKey(key), value); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetSetting("scheduling.owner.note", `"operator text"`); err != nil {
		t.Fatal(err)
	}
	all, err := st.AllSettings()
	if err != nil {
		t.Fatal(err)
	}
	for key := range facts {
		if _, listed := all[key]; listed {
			t.Errorf("AllSettings lists the fencing fact %q", key)
		}
		if !IsInternalSchedulingSetting(SettingKey(key)) {
			t.Errorf("%q is not recognized as a fencing fact", key)
		}
		if err := st.SetBusinessSetting(context.Background(), SettingKey(key), `"client"`); !errors.Is(err, ErrInternalSchedulingSetting) {
			t.Errorf("client write to %q = %v, want ErrInternalSchedulingSetting", key, err)
		}
		if got := st.GetSetting(SettingKey(key)); got != facts[key] {
			t.Errorf("%q = %q after a refused write, want %q", key, got, facts[key])
		}
	}
	if all["scheduling.owner.note"] != `"operator text"` {
		t.Errorf("operator key scheduling.owner.note = %q, want it listed unchanged", all["scheduling.owner.note"])
	}
	if IsInternalSchedulingSetting("scheduling.owner.note") {
		t.Error("an operator-defined scheduling.* key is treated as a fencing fact")
	}
	if err := st.CheckBusinessReads(context.Background()); err != nil {
		t.Errorf("readiness with fencing facts present: %v", err)
	}
}
