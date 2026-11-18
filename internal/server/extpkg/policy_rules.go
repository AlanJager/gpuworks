package extpkg

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Permissions this server's own policy asks of routes. The manifest schema
// lets a package declare routes with only the routes capability; this
// server also wants the administrator to approve serving them, and serving
// them without authentication, as permissions.
const (
	PermissionRoutes = "http:routes"
	PermissionPublic = "http:public"
)

// checkServerPolicy is this server's own policy, stricter than the schema
// and the same for every site: routes need their permissions, and an
// on_demand package, which is not running most of the time, may neither
// push state nor declare features (a feature it answers for would have no
// one to answer while it is stopped). It runs on install and on every
// recheck.
func checkServerPolicy(m *extapi.Manifest) error {
	for i, r := range m.Routes {
		field := "routes." + strconv.Itoa(i)
		if !slices.Contains(m.Permissions, PermissionRoutes) {
			return routePermissionMissing(PermissionRoutes, field)
		}
		if r.Access == extapi.RoutePublic && !slices.Contains(m.Permissions, PermissionPublic) {
			return routePermissionMissing(PermissionPublic, field)
		}
	}
	if m.Runtime == nil || m.Runtime.Lifecycle != extapi.LifecycleOnDemand {
		return nil
	}
	for _, p := range m.Permissions {
		if strings.HasPrefix(p, "push:") {
			return messages.NewProductError(messages.ErrorPluginStateOnDemand,
				fmt.Sprintf("%s starts on demand and may not hold the permission %s", m.ID, p),
				map[string]any{"id": m.ID, "permission": p})
		}
	}
	if len(m.Features) > 0 {
		return messages.NewProductError(messages.ErrorPluginStateOnDemand,
			fmt.Sprintf("%s starts on demand and may not declare the feature %s", m.ID, m.Features[0].Key),
			map[string]any{"id": m.ID, "feature": m.Features[0].Key})
	}
	return nil
}

// routePermissionMissing refuses a route whose permission the manifest
// does not ask for: rule is the permission, value the route.
func routePermissionMissing(perm, field string) error {
	return messages.NewProductError(messages.ErrorPluginPolicyRefused,
		fmt.Sprintf("%s needs the permission %s", field, perm),
		map[string]any{"rule": perm, "value": field})
}
