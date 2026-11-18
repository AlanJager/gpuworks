package pluginbridge

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func route(method, pattern string) extapi.RouteDecl {
	return extapi.RouteDecl{Method: method, Pattern: pattern, Action: "acme_backup.x", Access: extapi.RouteAdmin, Ungated: true}
}

func entryWith(id, version string, enabled bool, routes ...extapi.RouteDecl) extpkg.Entry {
	return extpkg.Entry{ID: id, Version: version, Control: layout.Control{Enabled: enabled},
		Verified: &extpkg.Verified{Manifest: &extapi.Manifest{ID: id, Version: version, Routes: routes}}}
}

func backupRoutes() []extapi.RouteDecl {
	long := route("POST", "/uploads")
	long.MaxBody, long.TimeoutS = 64<<20, 600
	return []extapi.RouteDecl{
		route("GET", "/"),
		route("GET", "/jobs"),
		route("POST", "/jobs"),
		route("GET", "/jobs/latest"),
		route("GET", "/jobs/{id}"),
		route("DELETE", "/jobs/{id}"),
		route("GET", "/jobs/{job}/logs/{line}"),
		route("GET", "/a/b/c"),
		route("GET", "/a/{x}/d"),
		long,
	}
}

func paramsOf(ps []Param) map[string]string {
	out := map[string]string{}
	for _, p := range ps {
		out[p.Name] = p.Value
	}
	return out
}

func TestRouteTableMatches(t *testing.T) {
	tbl := compileRoutes(map[string]extpkg.Entry{"acme_backup": entryWith("acme_backup", "1.0.0", true, backupRoutes()...)}, nil)
	cases := []struct {
		method, path, pattern string
		params                map[string]string
		result                MatchResult
	}{
		{"GET", "/acme_backup/", "/", map[string]string{}, MatchFound},
		{"GET", "/acme_backup/jobs", "/jobs", map[string]string{}, MatchFound},
		{"POST", "/acme_backup/jobs", "/jobs", map[string]string{}, MatchFound},
		{"GET", "/acme_backup/jobs/latest", "/jobs/latest", map[string]string{}, MatchFound},
		{"GET", "/acme_backup/jobs/7", "/jobs/{id}", map[string]string{"id": "7"}, MatchFound},
		{"DELETE", "/acme_backup/jobs/7", "/jobs/{id}", map[string]string{"id": "7"}, MatchFound},
		// Patterns that share a segment name it each their own way.
		{"GET", "/acme_backup/jobs/7/logs/12", "/jobs/{job}/logs/{line}", map[string]string{"job": "7", "line": "12"}, MatchFound},
		// A literal branch that ends without a route falls back to the
		// parameter, and the parameter it tried is dropped.
		{"GET", "/acme_backup/a/b/d", "/a/{x}/d", map[string]string{"x": "b"}, MatchFound},
		{"GET", "/acme_backup/a/b/c", "/a/b/c", map[string]string{}, MatchFound},
		// The method does not match.
		{"PUT", "/acme_backup/jobs/7", "", nil, MatchMethodNotAllowed},
		{"POST", "/acme_backup/jobs/latest", "", nil, MatchMethodNotAllowed},
		{"HEAD", "/acme_backup/jobs", "", nil, MatchMethodNotAllowed},
		// Misses, including every path that is not canonical.
		{"GET", "/acme_other/jobs", "", nil, MatchMiss},
		{"GET", "/acme_backup", "", nil, MatchMiss},
		{"GET", "/acme_backup/nope", "", nil, MatchMiss},
		{"GET", "/acme_backup/jobs/", "", nil, MatchMiss},
		{"GET", "/acme_backup//jobs", "", nil, MatchMiss},
		{"GET", "/acme_backup/jobs/./x", "", nil, MatchMiss},
		{"GET", "/acme_backup/jobs/..", "", nil, MatchMiss},
		{"GET", "/acme_backup/jobs/7/logs", "", nil, MatchMiss},
		{"GET", "acme_backup/jobs", "", nil, MatchMiss},
		{"GET", "", "", nil, MatchMiss},
	}
	for _, c := range cases {
		r, ps, res := tbl.Match(c.method, c.path)
		if res != c.result {
			t.Errorf("%s %s: result %d, want %d", c.method, c.path, res, c.result)
			continue
		}
		if c.result != MatchFound {
			if r != nil || ps != nil {
				t.Errorf("%s %s: a %d answered %+v %v", c.method, c.path, res, r, ps)
			}
			continue
		}
		if r.Decl.Pattern != c.pattern || r.Decl.Method != c.method || r.Plugin != "acme_backup" || r.Version != "1.0.0" {
			t.Errorf("%s %s matched %s %s", c.method, c.path, r.Decl.Method, r.Decl.Pattern)
		}
		if got := paramsOf(ps); len(got) != len(c.params) || len(ps) != len(c.params) {
			t.Errorf("%s %s: params %v, want %v", c.method, c.path, ps, c.params)
		} else {
			for k, v := range c.params {
				if got[k] != v {
					t.Errorf("%s %s: params %v, want %v", c.method, c.path, ps, c.params)
				}
			}
		}
	}
	// Limits default as the manifest says; declared ones are kept.
	r, _, _ := tbl.Match("GET", "/acme_backup/jobs")
	if r.MaxBody != extapi.DefaultRouteMaxBody || r.Timeout != extapi.DefaultRouteTimeoutS*time.Second || r.Down != "" {
		t.Fatalf("defaults: %+v", r)
	}
	r, _, _ = tbl.Match("POST", "/acme_backup/uploads")
	if r.MaxBody != 64<<20 || r.Timeout != 10*time.Minute {
		t.Fatalf("declared limits: %+v", r)
	}
	var nilTable *RouteTable
	if _, _, res := nilTable.Match("GET", "/acme_backup/jobs"); res != MatchMiss || nilTable.List() != nil {
		t.Fatal("an empty table matched")
	}
}

// Enabled packages serve their routes; disabled ones have none; revoked
// ones, and broken ones whose routes are known for that version, keep them
// answering as down.
func TestRouteTableFollowsPackageState(t *testing.T) {
	revoked := entryWith("acme_revoked", "1.0.0", true, route("GET", "/r"))
	revoked.Verified.Revoked = &extapi.Revocation{}
	broken := extpkg.Entry{ID: "acme_broken", Version: "2.0.0", Control: layout.Control{Enabled: true}, Err: errors.New("a file changed")}
	upgraded := extpkg.Entry{ID: "acme_upgraded", Version: "3.0.0", Control: layout.Control{Enabled: true}, Err: errors.New("a file changed")}
	unknown := extpkg.Entry{ID: "acme_unknown", Version: "1.0.0", Control: layout.Control{Enabled: true}, Err: errors.New("a file changed")}
	offRevoked := entryWith("acme_offrevoked", "1.0.0", false, route("GET", "/r"))
	offRevoked.Verified.Revoked = &extapi.Revocation{}
	offBroken := extpkg.Entry{ID: "acme_offbroken", Version: "2.0.0", Err: errors.New("a file changed")}
	on := entryWith("acme_on", "1.0.0", true, route("GET", "/r"))
	on.Verified.Manifest.Codes = []string{"ACME_ON_BUSY"}
	entries := map[string]extpkg.Entry{
		"acme_on":       on,
		"acme_off":      entryWith("acme_off", "1.0.0", false, route("GET", "/r")),
		"acme_none":     entryWith("acme_none", "1.0.0", true),
		"acme_revoked":  revoked,
		"acme_broken":   broken,
		"acme_upgraded": upgraded,
		"acme_unknown":  unknown,
		// Disabled wins over revoked and broken: absent either way.
		"acme_offrevoked": offRevoked,
		"acme_offbroken":  offBroken,
	}
	known := map[string]knownRoutes{
		"acme_broken":    {version: "2.0.0", decls: []extapi.RouteDecl{route("GET", "/r")}},
		"acme_upgraded":  {version: "2.0.0", decls: []extapi.RouteDecl{route("GET", "/r")}},
		"acme_offbroken": {version: "2.0.0", decls: []extapi.RouteDecl{route("GET", "/r")}},
	}
	tbl := compileRoutes(entries, known)
	want := map[string]string{"acme_on": "", "acme_revoked": StateRevoked, "acme_broken": StateBroken}
	for _, id := range []string{"acme_on", "acme_off", "acme_none", "acme_revoked", "acme_broken", "acme_upgraded", "acme_unknown", "acme_offrevoked", "acme_offbroken"} {
		r, _, res := tbl.Match("GET", "/"+id+"/r")
		down, present := want[id]
		switch {
		case present && (res != MatchFound || r.Down != down):
			t.Errorf("%s: %d %+v, want present and down %q", id, res, r, down)
		case !present && res != MatchMiss:
			t.Errorf("%s: %d, want absent", id, res)
		case present && (down == "") != slices.Equal(r.Codes, []string{"ACME_ON_BUSY"}):
			// A served route answers with its package's codes; a down
			// one never reaches the plugin.
			t.Errorf("%s: codes %v", id, r.Codes)
		}
	}
	var listed []string
	for _, r := range tbl.List() {
		listed = append(listed, r.Plugin)
	}
	if !slices.Equal(listed, []string{"acme_broken", "acme_on", "acme_revoked"}) {
		t.Fatalf("inventory = %v", listed)
	}
}

// A package that breaks keeps the routes it last verified with; once it
// is removed they are forgotten. A request keeps the table it loaded
// across a swap.
func TestRoutesSurviveBreakageAndRequestsKeepTheirTable(t *testing.T) {
	b := &Bridge{known: map[string]knownRoutes{}, entries: map[string]extpkg.Entry{
		"acme_backup": entryWith("acme_backup", "1.0.0", true, backupRoutes()...)}}
	b.compileRoutesLocked()
	before := b.Routes()
	b.entries["acme_backup"] = extpkg.Entry{ID: "acme_backup", Version: "1.0.0", Control: layout.Control{Enabled: true}, Err: errors.New("broken")}
	b.compileRoutesLocked()
	if r, _, _ := b.Routes().Match("GET", "/acme_backup/jobs/7"); r == nil || r.Down != StateBroken {
		t.Fatalf("broken package route = %+v", r)
	}
	if r, _, _ := before.Match("GET", "/acme_backup/jobs/7"); r == nil || r.Down != "" {
		t.Fatalf("the earlier table changed under its reader: %+v", r)
	}
	delete(b.entries, "acme_backup")
	b.compileRoutesLocked()
	if _, _, res := b.Routes().Match("GET", "/acme_backup/jobs/7"); res != MatchMiss || len(b.known) != 0 {
		t.Fatalf("a removed package's routes stayed: %d, known %v", res, b.known)
	}
}

// Match allocates nothing for a route without parameters and only the
// params slice for one with them.
func TestRouteMatchAllocations(t *testing.T) {
	tbl := compileRoutes(map[string]extpkg.Entry{"acme_backup": entryWith("acme_backup", "1.0.0", true, backupRoutes()...)}, nil)
	for path, bound := range map[string]float64{
		"/acme_backup/jobs/latest":     0,
		"/acme_backup/nope":            0,
		"/acme_backup/jobs/7":          1,
		"/acme_backup/jobs/7/logs/12":  1,
		"/acme_backup/a/b/d":           1,
		"/acme_backup/jobs/7/logs/x/y": 1,
	} {
		if n := testing.AllocsPerRun(200, func() { tbl.Match("GET", path) }); n > bound {
			t.Errorf("Match %s: %.0f allocations, want at most %.0f", path, n, bound)
		}
	}
}

// Readers match while the table is swapped (run under -race).
func TestRouteTableConcurrentSwaps(t *testing.T) {
	b := &Bridge{known: map[string]knownRoutes{}, entries: map[string]extpkg.Entry{
		"acme_backup": entryWith("acme_backup", "1.0.0", true, backupRoutes()...)}}
	b.compileRoutesLocked()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if r, ps, res := b.Routes().Match("GET", "/acme_backup/jobs/7"); res == MatchFound && (r.Plugin != "acme_backup" || ps[0].Value != "7") {
					t.Errorf("match = %+v %v", r, ps)
					return
				}
			}
		})
	}
	for n := range 200 {
		b.mu.Lock()
		b.entries["acme_backup"] = entryWith("acme_backup", "1.0.0", n%2 == 0, backupRoutes()...)
		b.compileRoutesLocked()
		b.mu.Unlock()
	}
	close(stop)
	wg.Wait()
}
