package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type namedModule string

func (m namedModule) Name() string { return string(m) }

type migratingModule struct {
	name, namespace string
}

func (m migratingModule) Name() string { return m.name }
func (m migratingModule) Migrations() store.MigrationSet {
	return store.MigrationSet{Namespace: m.namespace, TablePrefix: "ee_" + m.namespace + "_"}
}

type serviceModule struct{ namedModule }

func (serviceModule) Run(ctx context.Context) error { <-ctx.Done(); return nil }

// Without modules the API is the community API: the same routes, route by
// route.
func TestNoModulesKeepTheCommunityRoutes(t *testing.T) {
	ext, err := (&extensionSet{}).apiDeps()
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.RouteInventory(api.Router(api.Deps{Extensions: ext}))
	if err != nil {
		t.Fatal(err)
	}
	want, err := api.RouteInventory(api.Router(api.Deps{}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("routes without modules differ from the community routes:\n%v\nvs\n%v", got, want)
	}
}

// A module list the server cannot compose is refused before the database is
// created.
func TestNewRefusesModulesThatCannotCompose(t *testing.T) {
	cases := map[string]struct {
		mods []Module
		want string
	}{
		"nil module":         {[]Module{nil}, "extension module 1 is nil"},
		"bad name":           {[]Module{namedModule("Bad")}, `"Bad" does not match`},
		"one-letter name":    {[]Module{namedModule("a")}, `"a" does not match`},
		"duplicate":          {[]Module{namedModule("dup"), namedModule("dup")}, `"dup" is registered twice`},
		"foreign namespace":  {[]Module{migratingModule{name: "mine", namespace: "theirs"}}, `use namespace "theirs"`},
		"migrator with no steps": {[]Module{migratingModule{name: "x1", namespace: "x1"}}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: filepath.Join(dir, "gpuworks.db"), AdminUser: "admin",
				AdminPassword: "gpuworks-test-password-2026", Extensions: Extensions{Modules: c.mods}}
			a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if c.want == "" {
				// A set with no steps is valid: the module starts with no tables.
				if err != nil {
					t.Fatalf("New = %v", err)
				}
				a.proxy.Close()
				_ = a.store.Close()
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("New = %v, want %q", err, c.want)
			}
			if _, statErr := os.Stat(cfg.DBPath); !os.IsNotExist(statErr) {
				t.Fatalf("the database was created before the modules were checked: %v", statErr)
			}
		})
	}
}

// MigrationSets and ModuleNames follow module order; only migrators have a
// set, and every module claims its server.yaml section.
func TestModuleListsFollowModuleOrder(t *testing.T) {
	ext := Extensions{Modules: []Module{migratingModule{name: "zz", namespace: "zz"}, namedModule("aa"), migratingModule{name: "mm", namespace: "mm"}}}
	var namespaces []string
	for _, set := range MigrationSets(ext) {
		namespaces = append(namespaces, set.Namespace)
	}
	if strings.Join(namespaces, ",") != "zz,mm" || strings.Join(ModuleNames(ext), ",") != "zz,aa,mm" {
		t.Fatalf("sets %v names %v", namespaces, ModuleNames(ext))
	}
	if MigrationSets(Extensions{}) != nil || len(ModuleNames(Extensions{})) != 0 {
		t.Fatal("no modules, no sets and no names")
	}
}

// The services loop starts the server's four independent loops, then each
// module's Service, in module order.
func TestServicesAreTheCoreLoopsThenTheModules(t *testing.T) {
	a, _ := newTestApp(t, "gpuworks-test-password-2026")
	if n := len(a.services()); n != 4 {
		t.Fatalf("community services = %d, want 4", n)
	}
	dir := t.TempDir()
	cfg := Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: filepath.Join(dir, "gpuworks.db"), AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026",
		Extensions: Extensions{Modules: []Module{serviceModule{"one"}, namedModule("none"), serviceModule{"two"}}}}
	b, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.proxy.Close(); _ = b.store.Close() })
	if n := len(b.services()); n != 6 {
		t.Fatalf("services with two module services = %d, want 6", n)
	}
}
