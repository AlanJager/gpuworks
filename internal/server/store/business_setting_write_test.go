package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// A settings change from the API commits the row and its owed notification
// together, and a member that is closed or no longer eligible refuses it
// before anything is written.
func TestSetBusinessSettingCommitsWithOwedNotificationAndHonorsAdmission(t *testing.T) {
	events := bus.New()
	st, err := Open(filepath.Join(t.TempDir(), "settings.db"), events)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key := SettingKey("test.business.setting")
	// Opening the store seeds defaults; count hints from here.
	seeded := events.Generation("setting")

	if err := st.SetBusinessSetting(t.Context(), "", `"x"`); err == nil {
		t.Fatal("empty key accepted")
	}
	for revision, value := range []string{`"first"`, `"second"`} {
		if err := st.SetBusinessSetting(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
		if got := st.GetSetting(key); got != value {
			t.Fatalf("value=%q want %q", got, value)
		}
		var note CommittedNotification
		if err := st.db.First(&note, "request_key = ?", settingNotifyKey(string(key))).Error; err != nil {
			t.Fatal(err)
		}
		if note.Published || note.Revision != uint64(revision+1) || note.Topic != "setting" || note.ResourceID != string(key) || note.Reason != "updated" {
			t.Fatalf("owed notification=%+v", note)
		}
		if hints := events.Generation("setting") - seeded; hints != uint64(revision+1) {
			t.Fatalf("local hints=%d after %d committed writes", hints, revision+1)
		}
	}

	eligible := errors.New("member lost its material proof")
	var refuse error
	if err := st.InstallBusinessWriteBarrier(func(context.Context) error { return refuse }); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBusinessSetting(t.Context(), key, `"while closed"`); !errors.Is(err, ErrBusinessWritesClosed) {
		t.Fatalf("closed member wrote: %v", err)
	}
	if err := st.OpenBusinessWrites(); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBusinessSetting(t.Context(), key, `"open"`); err != nil {
		t.Fatal(err)
	}
	refuse = eligible
	if err := st.SetBusinessSetting(t.Context(), key, `"ineligible"`); !errors.Is(err, eligible) {
		t.Fatalf("ineligible member wrote: %v", err)
	}
	st.SuspendBusinessWrites()
	if err := st.SetBusinessSetting(t.Context(), key, `"suspended"`); !errors.Is(err, ErrBusinessWritesClosed) {
		t.Fatalf("suspended member wrote: %v", err)
	}
	if got := st.GetSetting(key); got != `"open"` {
		t.Fatalf("refused write changed the value: %q", got)
	}
	if hints := events.Generation("setting") - seeded; hints != 3 {
		t.Fatalf("refused writes published a hint: %d", hints)
	}
	var note CommittedNotification
	if err := st.db.First(&note, "request_key = ?", settingNotifyKey(string(key))).Error; err != nil || note.Revision != 3 {
		t.Fatalf("refused writes advanced the notification: %+v err=%v", note, err)
	}
}
