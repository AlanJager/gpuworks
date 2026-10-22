package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestAIKeyEncryption(t *testing.T) {
	box, err := auth.NewSecretbox("master")
	if err != nil {
		t.Fatal(err)
	}
	got, err := EncryptAIKey(box, "sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	if got == "sk-secret" || len(got) <= len(encryptedAIKeyPrefix) {
		t.Fatalf("key was not encrypted: %q", got)
	}
	plain, err := DecryptAIKey(box, got)
	if err != nil || plain != "sk-secret" {
		t.Fatalf("decrypt = %q, %v", plain, err)
	}
	legacy, err := DecryptAIKey(box, "legacy-key")
	if err != nil || legacy != "legacy-key" {
		t.Fatalf("legacy = %q, %v", legacy, err)
	}
	if _, err := DecryptAIKey(box, encryptedAIKeyPrefix+"broken"); err == nil {
		t.Fatal("malformed ciphertext accepted")
	}
}

func TestCreateAIConfigReplacesDefaultAtomically(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	first, err := s.CreateAIConfig(&AIConfig{Name: "first"})
	if err != nil || !first.IsDefault {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := s.CreateAIConfig(&AIConfig{Name: "second", IsDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	assertDefault := func(id uint) {
		t.Helper()
		rows, err := s.ListAIConfigs()
		if err != nil {
			t.Fatal(err)
		}
		defaults := 0
		for _, row := range rows {
			if row.IsDefault {
				defaults++
				if row.ID != id {
					t.Errorf("unexpected default %+v", row)
				}
			}
		}
		if defaults != 1 {
			t.Fatalf("defaults=%d rows=%+v", defaults, rows)
		}
	}
	assertDefault(second.ID)
	if _, err := s.CreateAIConfig(&AIConfig{Name: "ordinary"}); err != nil {
		t.Fatal(err)
	}
	assertDefault(second.ID)
	if err := s.DB().Exec("CREATE TRIGGER reject_ai BEFORE INSERT ON ai_configs WHEN NEW.name = 'rejected' BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAIConfig(&AIConfig{Name: "rejected", IsDefault: true}); err == nil {
		t.Fatal("insert failure ignored")
	}
	assertDefault(second.ID)
}
