package cli

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type tableModule struct{}

func (tableModule) Name() string { return "audit" }
func (tableModule) Migrations() store.MigrationSet {
	return store.MigrationSet{Namespace: "audit", TablePrefix: "ee_audit_", Migrations: []store.Migration{
		{Version: 1, Name: "events", Apply: func(tx *gorm.DB) error {
			return tx.Exec("CREATE TABLE ee_audit_events (id INTEGER PRIMARY KEY)").Error
		}},
		{Version: 2, Name: "actor", Apply: func(tx *gorm.DB) error {
			return tx.Exec("ALTER TABLE ee_audit_events ADD COLUMN actor TEXT").Error
		}},
	}}
}

// migrate runs the build's extension migrations after the core ones and
// reports each namespace's version beside the core version; a community
// build reports only the core.
func TestMigrateReportsEachExtensionNamespace(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gpuworks.db")
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	opt := Options{Extensions: app.Extensions{Modules: []app.Module{tableModule{}}}}
	if err := opt.migrate(log, []string{"--db", dbPath}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "msg=\"database migrated\"") || !strings.Contains(text, "msg=\"extension schema migrated\" namespace=audit version=2") {
		t.Fatalf("migrate log:\n%s", text)
	}
	out.Reset()
	if err := (Options{}).migrate(log, []string{"--db", dbPath}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "extension schema") {
		t.Fatalf("a community migrate reports no extension namespace:\n%s", out.String())
	}
	// An extra command opens the same database with the same migrations.
	st, err := (Env{Extensions: opt.Extensions}).OpenMaintenanceStore("", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, err := st.NamespaceVersion("audit"); err != nil || v != 2 {
		t.Fatalf("namespace version through Env = %d, %v", v, err)
	}
}

// Each module claims the server.yaml section under its name; any other
// section draws the warning instead of stopping the server.
func TestServeWarnsOnlyAboutSectionsNoModuleClaims(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(path, []byte("extensions:\n  audit:\n    keep: 30\n  leftover:\n    x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := serverconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	opt := Options{Extensions: app.Extensions{Modules: []app.Module{tableModule{}}}}
	warnUnclaimedSections(slog.New(slog.NewTextHandler(&out, nil)), &file, opt.claimedSections())
	if text := out.String(); !strings.Contains(text, "section=leftover") || strings.Contains(text, "section=audit") {
		t.Fatalf("warnings:\n%s", text)
	}
}

// A distribution profile for another edition than the build's is a defect of
// the build.
func TestOptionsRefuseAProfileForAnotherEdition(t *testing.T) {
	opt := Options{Edition: "acme"}
	opt.Distribution.Edition = "other"
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), `profile is for edition "other" but the build is "acme"`) {
			t.Fatalf("panic = %v", r)
		}
	}()
	opt.validate()
}
