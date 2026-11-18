package pluginbridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Caller is who a plugin route request comes from, as the server
// authenticated it.
type Caller struct {
	// Kind is extapi.PrincipalSession, PrincipalToken or PrincipalPublic;
	// a public caller carries no user.
	Kind     string
	UserID   uint64
	Username string
	// Role is the user's core role; admin makes the principal a platform
	// administrator.
	Role string
	// LocalAdmin is the effective break-glass flag; the principal carries
	// it only for an administrator's session.
	LocalAdmin bool
	// APIKeyID is the platform token's id, for kind token.
	APIKeyID uint64
	// RequestID is the server's id of the request; one too long or not
	// printable ASCII is clipped or replaced.
	RequestID string
	// Access is the access level of the route's feature now
	// (extapi.AccessFull or AccessReadOnly); empty for an ungated route.
	Access string
}

// Forward sends req, a request the server matched to route r and let
// through its checks, to r's plugin as a call of the route's class (admin
// for admin and admin_session routes, stream for stream routes, held to
// the route's timeout), signed with the principal of c for the process
// that serves it. req's path is already PathRoutePrefix plus the route
// path, its headers already stripped of anything a client may not send.
// The refusals are Call's.
func (b *Bridge) Forward(ctx context.Context, r *Route, req *http.Request, c Caller) (*http.Response, error) {
	class := pluginhost.Class{Admin: r.Decl.Access == extapi.RouteAdmin || r.Decl.Access == extapi.RouteAdminSession,
		Stream: r.Decl.Stream, Timeout: r.Timeout}
	return b.CallSigned(ctx, r.Plugin, req, class, signerFor(r, c))
}

// signerFor signs the principal of c on route r, expiring the route's
// timeout plus extapi.PrincipalExpirySlackS after it is signed.
func signerFor(r *Route, c Caller) pluginhost.Signer {
	p, access := principalFor(r, c)
	return func(key []byte, h http.Header) error {
		signed := *p
		signed.Exp = extapi.PrincipalExpiry(time.Now(), r.Decl.TimeoutS)
		hdr, mac, err := extapi.SignPrincipal(key, &signed, access)
		if err != nil {
			return err
		}
		h.Set(extapi.HeaderPrincipal, hdr)
		h.Set(extapi.HeaderPrincipalMac, mac)
		h.Set(extapi.HeaderFeatureAccess, access)
		return nil
	}
}

// principalFor is the principal of c on route r, without its expiry, and
// the feature access header value.
func principalFor(r *Route, c Caller) (*extapi.Principal, string) {
	p := &extapi.Principal{Kind: c.Kind, RequestID: requestID(c.RequestID), RouteAction: r.Decl.Action, Route: r.Decl.Pattern}
	if c.Kind != extapi.PrincipalPublic {
		p.UserID, p.Username, p.Roles = c.UserID, c.Username, []string{c.Role}
		p.PlatformAdmin = c.Role == "admin"
		p.LocalAdmin = c.LocalAdmin && p.PlatformAdmin && c.Kind == extapi.PrincipalSession
		if c.Kind == extapi.PrincipalToken {
			p.APIKeyID = c.APIKeyID
		}
	}
	access := ""
	if r.Decl.Feature != "" && c.Access != "" {
		access = extapi.FeatureAccess{r.Decl.Feature: c.Access}.String()
	}
	return p, access
}

// maxRequestID is the longest request id a principal carries.
const maxRequestID = 128

// requestID is id as a principal may carry it: clipped to maxRequestID
// bytes when it is printable ASCII, else (or when empty) a fresh one.
func requestID(id string) string {
	ok := id != ""
	for i := 0; ok && i < len(id); i++ {
		ok = id[i] > 0x20 && id[i] < 0x7f
	}
	if !ok {
		var b [16]byte
		_, _ = rand.Read(b[:])
		return hex.EncodeToString(b[:])
	}
	return id[:min(len(id), maxRequestID)]
}
