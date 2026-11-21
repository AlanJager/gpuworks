package aihelper

import (
	"regexp"
	"strings"
)

// routeDocs (apidocs_gen.go) is the one-line description docs/reference/api.md
// gives each GET route, keyed by normalizeRoute. TestRouteDocsMatchAPIReference
// keeps it equal to the document; regenerate it with make api-docs. The file
// is never edited or merged by hand: after a merge conflict in it, settle
// docs/reference/api.md and regenerate.

var routeParam = regexp.MustCompile(`\{[^}]*\}|\*`)

// normalizeRoute is a route's key whatever its parameters are called and
// whether the document spells a query: "/api/v1/nodes/{uuid}",
// "/api/v1/nodes/{id}" and "/api/v1/nodes/*" are all "/api/v1/nodes/{}".
func normalizeRoute(path string) string {
	path, _, _ = strings.Cut(path, "?")
	return routeParam.ReplaceAllString(strings.TrimSpace(path), "{}")
}

// RouteDoc is what docs/reference/api.md says a GET route answers, or "".
func RouteDoc(pattern string) string {
	return routeDocs[normalizeRoute(pattern)]
}
