package pluginbridge

import (
	"maps"
	"slices"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Route is one route an installed package declares, served at
// /api/v1/x/<Plugin><Decl.Pattern>.
type Route struct {
	Plugin  string
	Version string
	Decl    extapi.RouteDecl
	// MaxBody and Timeout are the declaration's limits with the defaults
	// filled in.
	MaxBody int64
	Timeout time.Duration
	// Down is the package state (StateBroken or StateRevoked) the route
	// answers 503 PLUGIN_UNAVAILABLE for without calling the plugin;
	// empty when the route is served.
	Down string

	// params names the pattern's {name} segments in order.
	params []string
}

// Param is one {name} segment of a matched path.
type Param struct {
	Name, Value string
}

// MatchResult says how a request path matched.
type MatchResult int

const (
	// MatchMiss: no route of any method has this path.
	MatchMiss MatchResult = iota
	// MatchFound: a route has this method and path.
	MatchFound
	// MatchMethodNotAllowed: routes have this path, none this method.
	MatchMethodNotAllowed
)

// RouteTable is the routes of every enabled package (served) and of every
// installed package that no longer verifies or is revoked (present, down).
// A disabled package has none. It is immutable: a new table replaces it as
// a whole, so a request keeps the table it matched against.
type RouteTable struct {
	plugins map[string]*pluginRoutes
	all     []*Route
}

type pluginRoutes struct {
	root node
	// params is the most {name} segments any of its patterns has.
	params int
}

// methods are the route methods, in the order of node.routes.
var methods = [...]string{"GET", "POST", "PUT", "PATCH", "DELETE"}

func methodIndex(m string) int {
	for i, x := range methods {
		if x == m {
			return i
		}
	}
	return -1
}

// node is one path segment of a package's patterns: literal children are
// tried before the parameter child.
type node struct {
	literal map[string]*node
	param   *node
	routes  [len(methods)]*Route
}

func (n *node) any() bool {
	for _, r := range n.routes {
		if r != nil {
			return true
		}
	}
	return false
}

// Match finds the route for method and path, where path is the request
// path below /api/v1/x: "/<package id><pattern path>". The path must be
// canonical (no empty, "." or ".." segments, no escapes); anything else
// misses. It takes no lock, costs one step per segment (and a step back
// when a literal branch ends without a route), and allocates at most the
// params slice, once, when the route has parameters.
func (t *RouteTable) Match(method, path string) (*Route, []Param, MatchResult) {
	if t == nil || len(path) < 2 || path[0] != '/' {
		return nil, nil, MatchMiss
	}
	id, rest, ok := strings.Cut(path[1:], "/")
	if !ok {
		return nil, nil, MatchMiss
	}
	p := t.plugins[id]
	if p == nil {
		return nil, nil, MatchMiss
	}
	var params []Param
	n := p.root.match(rest, &params, p.params)
	if n == nil {
		return nil, nil, MatchMiss
	}
	mi := methodIndex(method)
	if mi < 0 || n.routes[mi] == nil {
		return nil, nil, MatchMethodNotAllowed
	}
	r := n.routes[mi]
	for i := range params {
		params[i].Name = r.params[i]
	}
	return r, params, MatchFound
}

// match walks rest ("" for the pattern "/", else "seg/seg...") from n and
// returns the node holding routes for it, collecting the parameter values
// (Match names them after the route found, as patterns sharing a node may
// name a segment differently).
func (n *node) match(rest string, params *[]Param, capacity int) *node {
	if rest == "" {
		if n.any() {
			return n
		}
		return nil
	}
	seg, tail, more := strings.Cut(rest, "/")
	if seg == "" || seg == "." || seg == ".." || (more && tail == "") {
		return nil
	}
	if c := n.literal[seg]; c != nil {
		if found := c.match(tail, params, capacity); found != nil {
			return found
		}
	}
	if n.param != nil {
		if *params == nil {
			*params = make([]Param, 0, capacity)
		}
		at := len(*params)
		*params = append(*params, Param{Value: seg})
		if found := n.param.match(tail, params, capacity); found != nil {
			return found
		}
		*params = (*params)[:at]
	}
	return nil
}

// List is every route in the table, sorted by package, pattern and method.
func (t *RouteTable) List() []Route {
	if t == nil {
		return nil
	}
	out := make([]Route, len(t.all))
	for i, r := range t.all {
		out[i] = *r
	}
	return out
}

// compileRoutes builds the table: an enabled package that verifies serves
// its routes; one that is revoked, or no longer verifies, keeps the routes
// it last verified with (known, for this same version) and answers them as
// down; a disabled package has none.
func compileRoutes(entries map[string]extpkg.Entry, known map[string]knownRoutes) *RouteTable {
	t := &RouteTable{plugins: map[string]*pluginRoutes{}}
	for _, id := range slices.Sorted(maps.Keys(entries)) {
		e := entries[id]
		var decls []extapi.RouteDecl
		version, down := e.Version, ""
		switch {
		case e.Err != nil || e.Verified == nil:
			k, ok := known[id]
			if !ok || k.version != e.Version {
				continue
			}
			decls, down = k.decls, StateBroken
		case e.Verified.Revoked != nil:
			decls, down = e.Verified.Manifest.Routes, StateRevoked
		case !e.Control.Enabled:
			continue
		default:
			decls = e.Verified.Manifest.Routes
		}
		if len(decls) == 0 {
			continue
		}
		p := &pluginRoutes{}
		for _, d := range decls {
			r := &Route{Plugin: id, Version: version, Decl: d, MaxBody: d.MaxBody, Timeout: time.Duration(d.TimeoutS) * time.Second, Down: down}
			if r.MaxBody == 0 {
				r.MaxBody = extapi.DefaultRouteMaxBody
			}
			if d.TimeoutS == 0 {
				r.Timeout = extapi.DefaultRouteTimeoutS * time.Second
			}
			if p.add(r) {
				t.all = append(t.all, r)
			}
		}
		t.plugins[id] = p
	}
	slices.SortFunc(t.all, func(a, b *Route) int {
		if c := strings.Compare(a.Plugin, b.Plugin); c != 0 {
			return c
		}
		if c := strings.Compare(a.Decl.Pattern, b.Decl.Pattern); c != 0 {
			return c
		}
		return methodIndex(a.Decl.Method) - methodIndex(b.Decl.Method)
	})
	return t
}

// add puts r in the trie; the manifest's rules make every pattern valid
// and every method and shape unique, so a clash is only skipped.
func (p *pluginRoutes) add(r *Route) bool {
	mi := methodIndex(r.Decl.Method)
	if mi < 0 || r.Decl.Pattern == "" || r.Decl.Pattern[0] != '/' {
		return false
	}
	n := &p.root
	if r.Decl.Pattern != "/" {
		for seg := range strings.SplitSeq(r.Decl.Pattern[1:], "/") {
			if name, ok := strings.CutPrefix(seg, "{"); ok {
				r.params = append(r.params, strings.TrimSuffix(name, "}"))
				if n.param == nil {
					n.param = &node{}
				}
				n = n.param
				continue
			}
			if n.literal == nil {
				n.literal = map[string]*node{}
			}
			c := n.literal[seg]
			if c == nil {
				c = &node{}
				n.literal[seg] = c
			}
			n = c
		}
	}
	if n.routes[mi] != nil {
		return false
	}
	n.routes[mi] = r
	p.params = max(p.params, len(r.params))
	return true
}

// knownRoutes are the routes a package last verified with, kept while it
// stays installed so a package that breaks keeps answering them as down.
type knownRoutes struct {
	version string
	decls   []extapi.RouteDecl
}

// compileRoutesLocked remembers the routes of every entry that verifies,
// forgets those of removed packages and replaces the route table. The
// caller holds mu.
func (b *Bridge) compileRoutesLocked() {
	for id, e := range b.entries {
		if e.Err == nil && e.Verified != nil {
			b.known[id] = knownRoutes{version: e.Version, decls: e.Verified.Manifest.Routes}
		}
	}
	for id := range b.known {
		if _, ok := b.entries[id]; !ok {
			delete(b.known, id)
		}
	}
	b.routes.Store(compileRoutes(b.entries, b.known))
}

// Routes is the current route table: one atomic load, no lock. A caller
// keeps the table it loaded for the whole request.
func (b *Bridge) Routes() *RouteTable { return b.routes.Load() }
