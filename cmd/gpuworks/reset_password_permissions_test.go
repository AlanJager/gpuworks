package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

func TestMaintenancePolicyCarriesOnlyTheMatchingResolvedGroup(t *testing.T) {
	layout := selfinstall.NewLayout(t.TempDir(), t.TempDir(), "")
	db := filepath.Join(layout.DataDir, "gpuworks.db")
	other := filepath.Join(t.TempDir(), "other.db")
	for _, path := range []string{db, other} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	trusted, err := securefile.TrustedGroup(os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	resolve := func(candidate selfinstall.Layout) (securefile.Policy, error) {
		calls++
		if candidate.Dir != layout.Dir || candidate.DataDir != layout.DataDir {
			t.Fatalf("wrong installation sent to authorization: %+v", candidate)
		}
		return trusted, nil
	}
	dsn := (&url.URL{Scheme: "file", Path: db, RawQuery: "cache=shared"}).String()
	policy, matched, err := maintenancePolicyForDatabase(dsn, layout, resolve)
	if err != nil || !matched || policy != trusted || calls != 1 {
		t.Fatalf("resolved authorization was lost: policy=%v matched=%v calls=%d err=%v", policy, matched, calls, err)
	}
	if policy, matched, err := maintenancePolicyForDatabase(other, layout, resolve); err != nil || matched || policy.Mode() != securefile.OwnerMode || calls != 1 {
		t.Fatalf("unrelated database borrowed installation authority: policy=%v matched=%v calls=%d err=%v", policy, matched, calls, err)
	}
	wantErr := errors.New("cannot resolve authorized group")
	if _, matched, err := maintenancePolicyForDatabase(dsn, layout, func(selfinstall.Layout) (securefile.Policy, error) {
		return securefile.Policy{}, wantErr
	}); !matched || !errors.Is(err, wantErr) {
		t.Fatalf("authorization error was ignored: matched=%v err=%v", matched, err)
	}
}

func maintenanceServiceUnit(t *testing.T, layout selfinstall.Layout, binary string, withGroup bool) []byte {
	t.Helper()
	identity, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if identity.Uid == "0" || identity.Username == selfinstall.DefaultServiceUser {
		identity, err = user.Lookup("nobody")
		if err != nil {
			t.Fatal(err)
		}
	}
	metadata := "User=" + identity.Username + "\n"
	if withGroup {
		metadata += "Group=" + selfinstall.DefaultCredentialGroup + "\n"
	}
	unit := selfinstall.RenderUnit(selfinstall.UnitConfig{Binary: binary, ConfigPath: layout.ConfigPath(), DataDir: layout.DataDir})
	return []byte(strings.Replace(unit, "[Service]\n", "[Service]\n"+metadata, 1))
}

func TestMaintenancePolicyRequiresMatchingDatabaseServiceAndExplicitGroup(t *testing.T) {
	layout := selfinstall.NewLayout(t.TempDir(), t.TempDir(), "")
	layout.UnitDir = t.TempDir()
	dbPath := filepath.Join(layout.DataDir, "gpuworks.db")
	other := filepath.Join(t.TempDir(), "gpuworks.db")
	for _, path := range []string{dbPath, other} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unit := maintenanceServiceUnit(t, layout, layout.Binary(), true)
	if err := os.WriteFile(layout.UnitPath(), unit, 0o644); err != nil {
		t.Fatal(err)
	}
	uri := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "cache=shared"}).String()
	for _, dsn := range []string{dbPath, dbPath + "?cache=shared", uri} {
		policy, matched, err := maintenancePolicyForDatabase(dsn, layout, selfinstall.SensitiveFilePolicy)
		want := securefile.OwnerMode
		var gid int
		if runtime.GOOS == "linux" {
			group, lookupErr := user.LookupGroup(selfinstall.DefaultCredentialGroup)
			if lookupErr != nil {
				if !matched || err == nil || !strings.Contains(err.Error(), selfinstall.DefaultCredentialGroup) {
					t.Fatalf("missing authorized group was not fail-closed: matched=%v err=%v", matched, err)
				}
				t.Log("authorized group not installed: verified fail-closed group lookup")
				continue
			}
			gid, lookupErr = strconv.Atoi(group.Gid)
			if lookupErr != nil {
				t.Fatal(lookupErr)
			}
			want = securefile.GroupMode
		}
		if err != nil || !matched || policy.Mode() != want {
			t.Fatalf("bound service database %q: mode=%04o matched=%v err=%v", dsn, policy.Mode(), matched, err)
		}
		if actualGID, trusted := policy.GroupID(); runtime.GOOS == "linux" && (!trusted || actualGID != gid) {
			t.Fatalf("trusted service group identity not retained: got=%d trusted=%v want=%d", actualGID, trusted, gid)
		}
	}
	if policy, matched, err := maintenancePolicyForDatabase(other, layout, selfinstall.SensitiveFilePolicy); err != nil || matched || policy.Mode() != securefile.OwnerMode {
		t.Fatalf("unrelated explicit database inherited service access: policy=%v matched=%v err=%v", policy, matched, err)
	}
	for _, tc := range []struct {
		binary string
		group  bool
	}{
		{layout.Binary(), false},
		{filepath.Join(t.TempDir(), "other-server"), true},
	} {
		unit := maintenanceServiceUnit(t, layout, tc.binary, tc.group)
		if err := os.WriteFile(layout.UnitPath(), unit, 0o644); err != nil {
			t.Fatal(err)
		}
		if policy, matched, err := maintenancePolicyForDatabase(dbPath, layout, selfinstall.SensitiveFilePolicy); err != nil || !matched || policy.Mode() != securefile.OwnerMode {
			t.Fatalf("unverified service granted group access: policy=%v matched=%v err=%v", policy, matched, err)
		}
	}
}

func TestMaintenancePolicyDoesNotIgnoreUnreadableServiceMetadata(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("service-unit trust is only available on Linux")
	}
	layout := selfinstall.NewLayout(t.TempDir(), t.TempDir(), "")
	layout.UnitDir = t.TempDir()
	dbPath := filepath.Join(layout.DataDir, "gpuworks.db")
	if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(layout.UnitPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, matched, err := maintenancePolicyForDatabase(dbPath, layout, selfinstall.SensitiveFilePolicy); err == nil || !matched {
		t.Fatalf("service metadata read error was ignored: matched=%v err=%v", matched, err)
	}
}

func TestMaintenancePolicyUnknownDatabaseUsesOwnerOnly(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "unrelated.db")
	if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := maintenanceDatabasePolicy(dbPath)
	if err != nil || policy.Mode() != securefile.OwnerMode {
		t.Fatalf("unknown database policy=%v err=%v", policy, err)
	}
}

func TestMigrateKeepsStandaloneDatabasePrivate(t *testing.T) {
	for _, query := range []string{"", "?cache=shared"} {
		t.Run(fmt.Sprintf("query=%s", query), func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "standalone.db")
			for range 2 {
				if err := migrate(discardLog(), []string{"--db", dbPath + query}); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(dbPath)
				if err != nil || info.Mode().Perm() != securefile.OwnerMode {
					t.Fatalf("migration did not retain private standalone access: info=%v err=%v", info, err)
				}
			}
		})
	}
}
