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
	// Clearing the mark on the one default is refused: while any service is
	// stored, exactly one is the one the assistant sends to. Zero defaults is
	// the state the console cannot label and the assistant cannot read, and it
	// is reachable with one click on the switch, so the store answers the rule
	// instead of storing a state its own reader has no answer for.
	unmark := false
	if _, err := s.UpdateAIConfig(second.ID, AIConfigPatch{IsDefault: &unmark}); !errors.Is(err, ErrAIDefaultRequired) {
		t.Fatalf("clearing the only default = %v", err)
	}
	if everyDefault(t, s) != second.ID {
		t.Fatal("a refused patch cleared the default")
	}
	// Marking a different one first is the supported way to move it.
	if _, err := s.UpdateAIConfig(first.ID, AIConfigPatch{IsDefault: &mark}); err != nil {
		t.Fatal(err)
	}
	if everyDefault(t, s) != first.ID {
		t.Fatal("the default did not move to the named config")
	}
	// And the row that just lost the mark is now free to be left unmarked: the
	// invariant is about there being exactly one, not about which row carries it.
	if _, err := s.UpdateAIConfig(second.ID, AIConfigPatch{IsDefault: &unmark}); err != nil {
		t.Fatalf("clearing a non-default = %v", err)
	}
	if everyDefault(t, s) != first.ID {
		t.Fatal("clearing an ordinary row moved the default")
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

// Deleting the service the assistant uses hands the mark to the newest
// remaining one in the same transaction, so the assistant is never left with
// services stored and none in use. The newest is the row the console lists
// first, so what the operator sees after the delete is what the assistant uses.
func TestDeleteAIConfigPromotesTheNewestSurvivor(t *testing.T) {
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
	third, err := s.CreateAIConfig(&AIConfig{Name: "third"})
	if err != nil {
		t.Fatal(err)
	}
	// Deleting the default promotes the newest survivor, not the oldest.
	if promoted, err := s.DeleteAIConfig(first.ID); err != nil || promoted != third.ID {
		t.Fatalf("delete default: promoted=%d err=%v want %d", promoted, err, third.ID)
	}
	if everyDefault(t, s) != third.ID {
		t.Fatal("the deleted default was not replaced")
	}
	if _, err := s.AIConfigByID(first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted row still readable: %v", err)
	}
	// Deleting a row that is not the default leaves the default alone and
	// reports that nothing was promoted.
	if promoted, err := s.DeleteAIConfig(second.ID); err != nil || promoted != 0 {
		t.Fatalf("delete non-default: promoted=%d err=%v", promoted, err)
	}
	if everyDefault(t, s) != third.ID {
		t.Fatal("deleting an ordinary row moved the default")
	}
	// Deleting the last stored service leaves none, which is the one legal
	// zero-default state: nothing is configured.
	if promoted, err := s.DeleteAIConfig(third.ID); err != nil || promoted != 0 {
		t.Fatalf("delete last: promoted=%d err=%v", promoted, err)
	}
	if _, err := s.DefaultAIConfig(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("default after deleting everything = %v", err)
	}
	if s.HasAIConfig() {
		t.Fatal("a deleted service is still counted as configured")
	}
}

// A stale console deleting a row that is already gone is told so, instead of
// receiving success for a delete that removed nothing.
func TestDeleteAIConfigMissingRow(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	if _, err := s.DeleteAIConfig(404); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete of a missing config = %v", err)
	}
	if _, err := s.DeleteAIConfig(0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete of id 0 = %v", err)
	}
	kept, err := s.CreateAIConfig(&AIConfig{Name: "kept"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAIConfig(404); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a failed delete disturbed the store: %v", err)
	}
	if everyDefault(t, s) != kept.ID {
		t.Fatal("a failed delete cleared the default")
	}
}

// DefaultAIConfig reads the marked row and nothing else. It used to fall back to
// the OLDEST stored row when no mark was found, while the console lists the
// NEWEST first: the egress notice named a host the assistant's own traffic did
// not go to.
func TestDefaultAIConfigDoesNotEmptyFallBack(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	first, err := s.CreateAIConfig(&AIConfig{Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAIConfig(&AIConfig{Name: "second"}); err != nil {
		t.Fatal(err)
	}
	// Clear the mark behind the store's back, the state an older build could
	// leave behind: nothing may stand in for the missing default.
	if err := s.DB().Exec("UPDATE ai_configs SET is_default = 0").Error; err != nil {
		t.Fatal(err)
	}
	if c, err := s.DefaultAIConfig(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("default with no mark = %+v, %v", c, err)
	}
	mark := true
	if _, err := s.UpdateAIConfig(first.ID, AIConfigPatch{IsDefault: &mark}); err != nil {
		t.Fatal(err)
	}
	c, err := s.DefaultAIConfig()
	if err != nil || c.ID != first.ID {
		t.Fatalf("default = %+v, %v", c, err)
	}
}
