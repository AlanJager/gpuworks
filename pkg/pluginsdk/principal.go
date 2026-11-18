package pluginsdk

import (
	"context"
	"errors"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// caller is what RequirePrincipal verified for a request.
type caller struct {
	principal *extapi.Principal
	access    extapi.FeatureAccess
}

type callerKey struct{}

// RequirePrincipal serves h only for a route request whose principal the
// server signed under this spawn's key (extapi.VerifyPrincipal) and whose
// route_action is action, the action of the route the plugin itself matched.
// Wrap each route's handler where it is registered:
//
//	mux.Handle("POST /r/jobs", pluginsdk.RequirePrincipal("acme_backup.jobs_create", createJob))
//
// Comparing the two actions closes the gap between the server's router and
// the plugin's: a path the two match to different routes is refused rather
// than served under the other route's access and feature checks. Any other
// failure (no principal, a MAC that does not verify, an expired or
// malformed principal) is refused too, with 403 and a gwx/1 error marked
// with Gwx-Product-Error. The handler reads the caller with PrincipalFrom
// and FeatureAccessFrom.
func RequirePrincipal(action string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, refusal := verifyCaller(r, action, time.Now())
		if refusal != nil {
			w.Header().Set(extapi.HeaderProductError, "1")
			writeError(w, refusal)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

func verifyCaller(r *http.Request, action string, now time.Time) (*caller, *Error) {
	p := PluginFrom(r.Context())
	if p == nil || p.req == nil {
		return nil, Refuse(extapi.ReasonInternal, extapi.ErrorWireInvalid,
			"RequirePrincipal serves only requests the plugin's Config.Handler received",
			extapi.Params{"object": "principal", "field": "_root", "rule": extapi.RuleRelation})
	}
	principal, access, err := extapi.VerifyPrincipal(p.req.PrincipalKey,
		r.Header.Get(extapi.HeaderPrincipal), r.Header.Get(extapi.HeaderPrincipalMac), r.Header.Get(extapi.HeaderFeatureAccess), now)
	if err != nil {
		return nil, principalRefusal(err)
	}
	if principal.RouteAction != action {
		return nil, Refuse(extapi.ReasonPermissionDenied, extapi.ErrorWireInvalid,
			"the server matched route action "+principal.RouteAction+", the plugin "+action,
			extapi.Params{"object": "principal", "field": "route_action", "rule": extapi.RuleRelation})
	}
	return &caller{principal: principal, access: access}, nil
}

// principalRefusal is the 403 answer to a principal that does not verify.
func principalRefusal(err error) *Error {
	params := func(field, rule string) extapi.Params {
		return extapi.Params{"object": "principal", "field": field, "rule": rule}
	}
	switch {
	case errors.Is(err, extapi.ErrPrincipalMissing):
		return Refuse(extapi.ReasonPermissionDenied, extapi.ErrorWireInvalid, err.Error(), params("_root", extapi.RuleRequired))
	case errors.Is(err, extapi.ErrPrincipalMAC):
		// Not PLUGIN_SIGNATURE_INVALID: its text speaks of a changed file.
		return Refuse(extapi.ReasonPermissionDenied, extapi.ErrorWireInvalid, err.Error(), params("_root", extapi.RuleRelation))
	case errors.Is(err, extapi.ErrPrincipalExpired), errors.Is(err, extapi.ErrPrincipalTooLong):
		return Refuse(extapi.ReasonPermissionDenied, extapi.ErrorWireInvalid, err.Error(), params("exp", extapi.RuleRange))
	}
	return refusalOf(err, extapi.ReasonPermissionDenied)
}

// PrincipalFrom returns the caller of a request RequirePrincipal let
// through, or nil for any other request.
func PrincipalFrom(r *http.Request) *extapi.Principal {
	if c, ok := r.Context().Value(callerKey{}).(*caller); ok {
		return c.principal
	}
	return nil
}

// FeatureAccessFrom returns the access level of each feature the matched
// route declares, as the server signed it with the principal; nil when
// RequirePrincipal did not serve the request. A route the plugin declared
// ungated has none.
func FeatureAccessFrom(r *http.Request) extapi.FeatureAccess {
	if c, ok := r.Context().Value(callerKey{}).(*caller); ok {
		return c.access
	}
	return nil
}
