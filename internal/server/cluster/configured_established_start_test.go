package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These synthetic persisted YAML fixtures exercise input validation only;
// canceled startup does not claim a live database, membership or quorum.
func configuredEstablishedStartFixture(t *testing.T, members string) configuredFixture {
	t.Helper()
	f := newConfiguredFixture(t)
	if err := f.prepare(); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"info.yaml": "ID: 1\nAddress: " + f.address + "\n", "cluster.yaml": members} {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestConfiguredEstablishedStartRequiresDurableMultiMemberIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	t.Run("no-transport", func(t *testing.T) {
		dir := t.TempDir()
		db, err := StartEstablishedMemberDatabase(ctx, dir, "test.db")
		if db != nil || err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) {
			t.Fatalf("missing durable identity reached startup: %v/%v", db, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("plaintext fallback changed directory: %v/%v", entries, err)
		}
	})
	for _, tc := range []struct{ name, members, want string }{
		{"single-member", "- ID: 1\n  Address: 127.0.0.1:19877\n", "established matching membership"},
		{"local-mismatch", "- ID: 2\n  Address: 127.0.0.1:19877\n- ID: 3\n  Address: 127.0.0.1:19878\n", "unique matching persisted member"},
		{"remote-duplicate-id", "- ID: 1\n  Address: 127.0.0.1:19877\n- ID: 2\n  Address: 127.0.0.1:19878\n- ID: 2\n  Address: 127.0.0.1:19879\n", "duplicate persisted membership"},
		{"remote-duplicate-address", "- ID: 1\n  Address: 127.0.0.1:19877\n- ID: 2\n  Address: 127.0.0.1:19878\n- ID: 3\n  Address: 127.0.0.1:19878\n", "duplicate persisted membership"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := configuredEstablishedStartFixture(t, tc.members)
			db, err := StartEstablishedMemberDatabase(ctx, f.dir, "test.db")
			if db != nil || err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, context.Canceled) || errors.Is(err, ErrUnavailable) {
				t.Fatalf("invalid persisted membership reached startup: %v/%v", db, err)
			}
		})
	}
}

func TestConfiguredEstablishedStartValidatesBeforeCanceledStartup(t *testing.T) {
	f := configuredEstablishedStartFixture(t, "- ID: 1\n  Address: 127.0.0.1:19877\n- ID: 2\n  Address: 127.0.0.1:19878\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, open := range []struct {
		name string
		call func(context.Context, string, string) (*Database, error)
	}{
		{"start-only", StartEstablishedMemberDatabase}, {"wait-open", OpenEstablishedMemberDatabase},
	} {
		t.Run(open.name, func(t *testing.T) {
			db, err := open.call(ctx, f.dir, "test.db")
			if db != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("valid established material did not select authorized startup: %v/%v", db, err)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(f.dir, "join"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := StartEstablishedMemberDatabase(ctx, f.dir, "test.db")
	if db != nil || err == nil || !strings.Contains(err.Error(), "incomplete join state") || errors.Is(err, context.Canceled) {
		t.Fatalf("pending native join reached authorized startup: %v/%v", db, err)
	}
}
