package store

import (
	"errors"
	"testing"
)

// everyDefault fails a config that is not the only default, and returns the
// default's id.
func everyDefault(t *testing.T, s *Store) uint {
	t.Helper()
	rows, err := s.ListAIConfigs()
	if err != nil {
		t.Fatal(err)
	}
	defaults := 0
	var id uint
	for _, row := range rows {
		if row.IsDefault {
			defaults++
			id = row.ID
		}
	}
	if defaults != 1 {
		t.Fatalf("defaults=%d rows=%+v", defaults, rows)
	}
	return id
}

// A patch changes the fields it names and leaves every other field as stored,
// so an edit of one field cannot wipe the rest.
func TestUpdateAIConfigAppliesOnlyNamedFields(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	created, err := s.CreateAIConfig(&AIConfig{
		Name: "primary", Preset: "deepseek", BaseURL: "https://api.deepseek.com",
		APIKey: "enc:v1:kept", Model: "deepseek-v4-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	name := "renamed"
	updated, err := s.UpdateAIConfig(created.ID, AIConfigPatch{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "renamed" || updated.Preset != "deepseek" || updated.BaseURL != "https://api.deepseek.com" ||
		updated.APIKey != "enc:v1:kept" || updated.Model != "deepseek-v4-flash" || !updated.IsDefault {
		t.Fatalf("a name change altered other fields: %+v", updated)
	}
	stored, err := s.AIConfigByID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *stored != *updated {
		t.Fatalf("returned row %+v differs from the stored one %+v", updated, stored)
	}
	// A patch naming nothing writes nothing and keeps the row intact.
	if _, err := s.UpdateAIConfig(created.ID, AIConfigPatch{}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.AIConfigByID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *unchanged != *stored {
		t.Fatalf("an empty patch changed the row: %+v", unchanged)
	}
}

// Making one config the default clears the mark on the others in the same
// transaction; the assistant must never see two defaults.
func TestUpdateAIConfigKeepsOneDefault(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	first, err := s.CreateAIConfig(&AIConfig{Name: "first"})
	if err != nil || !first.IsDefault {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := s.CreateAIConfig(&AIConfig{Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if everyDefault(t, s) != first.ID {
		t.Fatal("the first config should stay the default")
	}
	mark := true
	if _, err := s.UpdateAIConfig(second.ID, AIConfigPatch{IsDefault: &mark}); err != nil {
		t.Fatal(err)
	}
	if everyDefault(t, s) != second.ID {
		t.Fatal("marking a config the default did not clear the old one")
	}
	// Clearing it leaves none: the product is usable without AI, and the
	// assistant then falls back to any config (DefaultAIConfig).
	unmark := false
	if _, err := s.UpdateAIConfig(second.ID, AIConfigPatch{IsDefault: &unmark}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAIConfigs()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.IsDefault {
			t.Fatalf("no config should be the default: %+v", rows)
		}
	}
}

// A failure while writing rolls the whole patch back: the row keeps its values
// and the previous default keeps its mark.
func TestUpdateAIConfigRollsBack(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	first, err := s.CreateAIConfig(&AIConfig{Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateAIConfig(&AIConfig{Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Exec("CREATE TRIGGER reject_ai_update BEFORE UPDATE ON ai_configs WHEN NEW.name = 'rejected' BEGIN SELECT RAISE(ABORT, 'injected update failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	mark, name := true, "rejected"
	if _, err := s.UpdateAIConfig(second.ID, AIConfigPatch{Name: &name, IsDefault: &mark}); err == nil {
		t.Fatal("update failure ignored")
	}
	if everyDefault(t, s) != first.ID {
		t.Fatal("a failed update left the old default cleared")
	}
	stored, err := s.AIConfigByID(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "second" || stored.IsDefault {
		t.Fatalf("a failed update kept part of the patch: %+v", stored)
	}
}

func TestUpdateAIConfigMissingRow(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	name := "gone"
	if _, err := s.UpdateAIConfig(404, AIConfigPatch{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing config = %v", err)
	}
	if _, err := s.AIConfigByID(404); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read of a missing config = %v", err)
	}
}

// A config written before the key was encrypted keeps its plaintext value
// through a patch that does not name the key: re-encrypting it would need the
// operator to retype a secret they never saw, and dropping it would break the
// assistant.
func TestUpdateAIConfigKeepsLegacyPlaintextKey(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	created, err := s.CreateAIConfig(&AIConfig{Name: "legacy", APIKey: "sk-plaintext"})
	if err != nil {
		t.Fatal(err)
	}
	model := "m"
	if _, err := s.UpdateAIConfig(created.ID, AIConfigPatch{Model: &model}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.AIConfigByID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.APIKey != "sk-plaintext" {
		t.Fatalf("key changed to %q", stored.APIKey)
	}
}
