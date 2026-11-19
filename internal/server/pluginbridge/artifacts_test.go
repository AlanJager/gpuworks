//go:build linux || darwin

package pluginbridge

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// memoryFields classifies every map of the bridge, the plugin host and
// pushed state, and the other fields that hold something per package: the
// per-package artifact it is (PerPackageArtifacts), or why it is not one
// ("-" and the reason).
var memoryFields = map[string]string{
	"Bridge.entries":    "memory:bridge.entries",
	"Bridge.pushers":    "memory:bridge.pushers",
	"Bridge.refusals":   "memory:bridge.refusals",
	"Bridge.crashloops": "memory:bridge.crashloops",
	"Bridge.known":      "memory:bridge.known",
	"Bridge.pending":    "memory:bridge.pending",
	"Bridge.routes":     "memory:bridge.routes",
	"Bridge.banners":    "memory:bridge.banners",
	"Bridge.cfg":        "memory:features",

	"Host.plugins": "memory:host.plugins",
	"Host.metrics": "memory:host.metrics",
	"Host.logs":    "memory:host.logs",

	"State.parts":      "memory:state.parts",
	"State.spawn":      "memory:state.spawn",
	"State.ready":      "memory:state.ready",
	"State.down":       "memory:state.down",
	"State.downDirty":  "memory:state.downDirty",
	"State.overrides":  "memory:state.overrides",
	"State.superseded": "memory:state.superseded",
	"State.reg":        "memory:state.reg",
	"State.pending":    "- a push waiting for the writer's next commit, which takes it",
	"State.inflight":   "- the generation of a push being committed, dropped when the commit returns",
	"State.heldByCap":  "- keyed by node id: the capacity selection's own outcome",
}

// notPerPackageTables are the pstate_* tables that hold nothing of one
// package.
var notPerPackageTables = map[string]string{
	"pstate_clock":  "the plugin clock's high-water mark",
	"pstate_marker": "the version markers of the stored forms of each kind",
}

func artifactNames() map[string]Artifact {
	out := map[string]Artifact{}
	for _, a := range PerPackageArtifacts {
		out[a.Name] = a
	}
	return out
}

// Every map of the bridge, the plugin host and pushed state is classified,
// and every in-memory artifact names a field; a new map keyed by package
// id fails here until it joins PerPackageArtifacts (and the uninstall that
// empties it).
func TestEveryPerPackageStructureIsListed(t *testing.T) {
	names := artifactNames()
	used := map[string]bool{}
	for _, typ := range []reflect.Type{reflect.TypeFor[Bridge](), reflect.TypeFor[pluginhost.Host](), reflect.TypeFor[pushstate.State]()} {
		for i := range typ.NumField() {
			f := typ.Field(i)
			key := typ.Name() + "." + f.Name
			class, listed := memoryFields[key]
			if f.Type.Kind() == reflect.Map && !listed {
				t.Errorf("%s is a map no entry of memoryFields classifies: add it to PerPackageArtifacts (and empty it on uninstall) or say why it holds nothing per package", key)
				continue
			}
			if !listed || strings.HasPrefix(class, "-") {
				continue
			}
			if names[class].Where != ArtifactMemory {
				t.Errorf("%s is classified as %q, which PerPackageArtifacts does not list as in memory", key, class)
			}
			used[class] = true
		}
	}
	for key := range memoryFields {
		typ, field, _ := strings.Cut(key, ".")
		var found bool
		for _, rt := range []reflect.Type{reflect.TypeFor[Bridge](), reflect.TypeFor[pluginhost.Host](), reflect.TypeFor[pushstate.State]()} {
			if rt.Name() == typ {
				_, found = rt.FieldByName(field)
			}
		}
		if !found {
			t.Errorf("memoryFields names %s, which no longer exists", key)
		}
	}
	for _, a := range PerPackageArtifacts {
		if a.Where == ArtifactMemory && !used[a.Name] {
			t.Errorf("in-memory artifact %s names no field", a.Name)
		}
		if where, _, ok := strings.Cut(a.Name, ":"); !ok || where != a.Where {
			t.Errorf("artifact %s is listed as %s", a.Name, a.Where)
		}
	}
}

// Every pstate_* table is a per-package artifact, or holds nothing of one
// package; every table artifact exists.
func TestEveryPushedStateTableIsListed(t *testing.T) {
	w := newWorld(t, nil)
	var tables []string
	if err := w.st.DB().Raw(`SELECT name FROM sqlite_master WHERE type = 'table' AND (name LIKE 'pstate\_%' ESCAPE '\' OR name = 'plugin_markers') ORDER BY name`).Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	if len(tables) < 10 {
		t.Fatalf("pstate tables = %v", tables)
	}
	names := artifactNames()
	for _, table := range tables {
		if _, ok := notPerPackageTables[table]; ok {
			continue
		}
		if a, ok := names["db:"+table]; !ok || a.Where != ArtifactDB {
			t.Errorf("table %s is not in PerPackageArtifacts: add it (and remove its rows on uninstall) or say why it holds nothing per package", table)
		}
	}
	for _, a := range PerPackageArtifacts {
		if a.Where == ArtifactDB && !slices.Contains(tables, strings.TrimPrefix(a.Name, "db:")) {
			t.Errorf("artifact %s names no table", a.Name)
		}
	}
}

// After an uninstall no per-package structure in memory mentions the
// package: its entry, its started identity, its route table entries, its
// health, its instance, counters and logs, and every part, liveness record
// and override pushed state held of it. Another package keeps its own.
func TestUninstallLeavesNothingInMemory(t *testing.T) {
	w := newWorld(t, nil)
	sp := &sdkSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}
	var changed atomic.Int32
	b := w.openHealth(sp, callTiming(), &changed)
	withHolds := func(s *extsign.Spec) {
		s.Manifest.Permissions = []string{"http:routes", "push:node_holds"}
		s.Manifest.Uses = map[string]int{"table.node_holds": 1}
		s.Manifest.Requires = map[string]int{extapi.CapRoutes: 1}
		s.Manifest.Routes = []extapi.RouteDecl{{Method: http.MethodGet, Pattern: "/status", Action: "acme_backup.status", Access: extapi.RouteAdmin, Ungated: true}}
		s.Manifest.Features = []extapi.FeatureDecl{{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf}}
	}
	install(t, b, w.pkg("acme_backup", "1.0.0", withHolds), false)
	install(t, b, w.pkg("acme_driver", "1.0.0", nil), true)
	pushHolds(t, b, w, "acme_backup", 1, holdOf(3))
	if _, err := b.Override(context.Background(), OverrideRequest{Owner: "acme_backup", Kind: extapi.KindNodeHolds, Entry: "3", Action: "delete",
		Reason: "relief", By: alice}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enable(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	waitState(t, b, "acme_driver", string(pluginhost.StateRunning))
	if left := memoryMentions(b, "acme_backup"); len(left) < 8 {
		t.Fatalf("before the uninstall only %v mention the package; the check would prove nothing", left)
	}

	if err := uninstallConfirmed(t, b, context.Background(), "acme_backup", true); err != nil {
		t.Fatal(err)
	}
	if left := memoryMentions(b, "acme_backup"); len(left) != 0 {
		t.Fatalf("after the uninstall %v still mention acme_backup", left)
	}
	if left := memoryMentions(b, "acme_driver"); !slices.Contains(left, "memory:bridge.entries") || !slices.Contains(left, "memory:host.plugins") {
		t.Fatalf("the other package lost its own state: %v", left)
	}
}

// memoryMentions names the per-package artifacts in memory that hold
// something mentioning id.
func memoryMentions(b *Bridge, id string) []string {
	var out []string
	check := func(name string, v reflect.Value) {
		if mentions(v, id, 0) && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for key, class := range memoryFields {
		if strings.HasPrefix(class, "-") || key == "Bridge.cfg" || key == "Bridge.routes" || key == "Bridge.banners" {
			continue
		}
		typ, field, _ := strings.Cut(key, ".")
		var v reflect.Value
		switch typ {
		case "Bridge":
			v = reflect.ValueOf(b).Elem().FieldByName(field)
		case "Host":
			v = reflect.ValueOf(b.host).Elem().FieldByName(field)
		case "State":
			v = reflect.ValueOf(b.state).Elem().FieldByName(field)
		}
		check(class, v)
	}
	check("memory:bridge.routes", reflect.ValueOf(b.Routes().List()))
	check("memory:bridge.banners", reflect.ValueOf(b.HealthBanners()))
	for _, f := range b.state.Features() {
		if b.state.FeatureOwner(f.Key) == id {
			out = append(out, "memory:features")
		}
	}
	slices.Sort(out)
	return out
}

// mentions reports whether v holds the string id (a map key, a field, an
// element), following pointers, maps, slices and structs a few levels
// deep. Values are read through reflection without Interface, so
// unexported fields count too.
func mentions(v reflect.Value, id string, depth int) bool {
	if depth > 6 || !v.IsValid() {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		return v.String() == id || strings.HasPrefix(v.String(), id+".") || strings.HasSuffix(v.String(), "."+id)
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && mentions(v.Elem(), id, depth+1)
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if mentions(iter.Key(), id, depth+1) || mentions(iter.Value(), id, depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if mentions(v.Index(i), id, depth+1) {
				return true
			}
		}
	case reflect.Struct:
		if v.Type().PkgPath() == "sync" || v.Type().PkgPath() == "sync/atomic" {
			return false
		}
		for i := range v.NumField() {
			if mentions(v.Field(i), id, depth+1) {
				return true
			}
		}
	}
	return false
}
