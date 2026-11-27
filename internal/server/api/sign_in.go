package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

// Sign-in through an identity connector (ADR 0085 decision 3). The browser
// starts at the core, which binds the sign-in to it with a one-time cookie
// and hands the connector a handle derived from the binding; the connector
// gets a ticket bound to that handle and sends the browser back to the core,
// which redeems the ticket only in the browser that holds the binding.

// SignInOption is a sign-in entry an extension offers (app.Host.OfferSignIn):
// Start is one of its AccessPublic GET routes, relative to /api/v1, and the
// entry follows that route's feature.
type SignInOption struct {
	Source string
	Label  string
	Icon   string
	Start  string
	Module string
}

// SignInRedeemer redeems a sign-in ticket for its account and answers the
// key that derives browser-binding handles.
type SignInRedeemer interface {
	RedeemSignInTicket(ticket, handle string) (user *store.User, source string, err error)
	BindingKey() []byte
}

const (
	// signInBindingCookie holds the browser binding of a sign-in in
	// progress, for /api/v1/auth only.
	signInBindingCookie = "gpuworks_sign_in"
	signInBindingPath   = "/api/v1/auth"
	signInBindingTTL    = 10 * time.Minute
	// signInFailurePath is the console page a refused redemption lands on,
	// with ?code=<CODE>; the sign-in page shows the code's text.
	signInFailurePath = "/login"
)

// signInRoutes mounts the sign-in routes, unauthenticated.
func (a *apiHandlers) signInRoutes(mux interface {
	Get(string, http.HandlerFunc)
}) {
	mux.Get("/auth/sign-in-options", a.signInOptions)
	mux.Get("/auth/sign-in/start", a.signInStart)
	mux.Get("/auth/complete", a.signInComplete)
}

// signInEntry is source's option while its start route's feature is
// available (full or read-only); a locked or hidden feature has none.
func (a *apiHandlers) signInEntry(source string) (SignInOption, bool) {
	for _, o := range a.d.Extensions.SignIns {
		if o.Source != source {
			continue
		}
		spec, ok := a.signInStartSpec(o)
		if !ok {
			return SignInOption{}, false
		}
		if spec.Feature != "" {
			if access := a.d.Extensions.Features.Access(spec.Feature); access != featuregate.Full && access != featuregate.ReadOnly {
				return SignInOption{}, false
			}
		}
		return o, true
	}
	return SignInOption{}, false
}

// signInStartSpec is the route spec of o's start route.
func (a *apiHandlers) signInStartSpec(o SignInOption) (RouteSpec, bool) {
	spec, ok := a.publicGets[o.Start]
	return spec, ok
}

func (a *apiHandlers) signInOptions(w http.ResponseWriter, _ *http.Request) {
	out := dto.SignInOptions{LocalPassword: a.sessionPolicy.Normalized().LocalPassword, Entries: []dto.SignInEntry{}}
	for _, o := range a.d.Extensions.SignIns {
		if _, ok := a.signInEntry(o.Source); !ok {
			continue
		}
		out.Entries = append(out.Entries, dto.SignInEntry{Source: o.Source, Label: o.Label, Icon: o.Icon,
			URL: "/api/v1/auth/sign-in/start?source=" + url.QueryEscape(o.Source)})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// signInStart binds a new sign-in to this browser and sends it to the
// connector's start route. It never redirects anywhere but a declared start
// route of an available entry.
func (a *apiHandlers) signInStart(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	o, ok := a.signInEntry(source)
	if !ok || a.d.Extensions.Accounts == nil {
		writeProductError(w, http.StatusNotFound, messages.ErrorIdentitySourceUnavailable,
			"no sign-in through this identity source is available", map[string]any{"source": source})
		return
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	binding := hex.EncodeToString(raw[:])
	http.SetCookie(w, &http.Cookie{Name: signInBindingCookie, Value: binding, Path: signInBindingPath, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: a.managementCookieSecure(r), MaxAge: int(signInBindingTTL / time.Second),
		Expires: time.Now().UTC().Add(signInBindingTTL)})
	target := url.URL{Path: "/api/v1" + o.Start, RawQuery: url.Values{"handle": {a.signInHandle(binding)}}.Encode()}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// signInHandle is the handle of a binding: an HMAC under the binding key, so
// it cannot be turned back into the binding.
func (a *apiHandlers) signInHandle(binding string) string {
	mac := hmac.New(sha256.New, a.d.Extensions.Accounts.BindingKey())
	mac.Write([]byte(binding))
	return hex.EncodeToString(mac.Sum(nil))
}

// signInComplete redeems a ticket in the browser that started the sign-in:
// it looks only at the binding cookie, not at Sec-Fetch-Site or Origin,
// because the return from the identity source is a cross-site top-level GET
// (ADR 0085 decision 3). It answers by redirect, to the console on success and
// to the sign-in page with ?code=<CODE> on a refusal.
func (a *apiHandlers) signInComplete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// The binding is spent whatever happens next.
	http.SetCookie(w, &http.Cookie{Name: signInBindingCookie, Value: "", Path: signInBindingPath, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: a.managementCookieSecure(r), MaxAge: -1, Expires: time.Unix(0, 0)})
	fail := func(code string) {
		a.usageCounters().Inc(usagereport.KindLogin, "failed")
		http.Redirect(w, r, signInFailurePath+"?code="+url.QueryEscape(code), http.StatusFound)
	}
	cookie, err := r.Cookie(signInBindingCookie)
	if err != nil || cookie.Value == "" || a.d.Extensions.Accounts == nil || a.d.Store == nil {
		fail(messages.ErrorIdentityTicketInvalid)
		return
	}
	u, source, err := a.d.Extensions.Accounts.RedeemSignInTicket(r.URL.Query().Get("ticket"), a.signInHandle(cookie.Value))
	switch {
	case errors.Is(err, store.ErrUserDisabled):
		fail(messages.ErrorUserDisabled)
		return
	case err != nil:
		fail(messages.ErrorIdentityTicketInvalid)
		return
	}
	// The source's sign-in may have been withdrawn (its feature locked)
	// since the ticket was issued.
	if _, ok := a.signInEntry(source); !ok {
		fail(messages.ErrorIdentitySourceUnavailable)
		return
	}
	policy := a.sessionPolicy.Normalized()
	credential, session, err := a.d.Store.CreateManagementSession(u, policy.IdleTimeout, policy.MaxLifetime)
	if err != nil {
		fail(messages.ErrorIdentityTicketInvalid)
		return
	}
	a.setManagementCookie(w, r, credential, session.AbsoluteExpiresAt)
	a.usageCounters().Inc(usagereport.KindLogin, "ok")
	http.Redirect(w, r, "/", http.StatusFound)
}
