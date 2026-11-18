package extapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The headers the server sets on a request it forwards to a plugin route
// (POST and the rest on /r/<path> of the plugin's serve connection). The
// server strips any client-sent header of these names first.
const (
	// HeaderPrincipal carries who is calling: a Principal in canonical
	// JSON, base64url-encoded without padding.
	HeaderPrincipal = "Gwx-Principal"
	// HeaderPrincipalMac authenticates HeaderPrincipal and
	// HeaderFeatureAccess together with the per-spawn principal key
	// (SignPrincipal).
	HeaderPrincipalMac = "Gwx-Principal-Mac"
	// HeaderFeatureAccess lists the access level of each feature the
	// matched route declares (FeatureAccess); an ungated route sends it
	// empty.
	HeaderFeatureAccess = "Gwx-Feature-Access"
	// PathRoutePrefix prefixes a route's path on the serve connection: the
	// server forwards /api/v1/x/<id><path> to PathRoutePrefix+<path>.
	PathRoutePrefix = "/r"
)

const objectPrincipal = "principal"

// Principal kinds: how the caller authenticated.
const (
	PrincipalSession = "session" // a console login session
	PrincipalToken   = "token"   // a platform token, on behalf of its user
	PrincipalPublic  = "public"  // nobody: a public route
)

// Principal limits.
const (
	// MaxPrincipalBytes bounds a Principal's canonical JSON.
	MaxPrincipalBytes = 8 << 10
	// PrincipalExpirySlackS is added to a route's timeout to give a
	// principal's expiry (PrincipalExpiry).
	PrincipalExpirySlackS = 5
	maxUsername           = 128
	maxRequestID          = 128
)

// A project a caller's data scope covers.
var projectRefPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Principal is who calls a plugin route, as the server authenticated and
// matched the request. The plugin trusts it only after VerifyPrincipal.
type Principal struct {
	// Kind is session, token or public. A public principal carries no
	// user: UserID, Username, Roles, ScopeProjects, APIKeyID and both admin
	// flags are empty.
	Kind     string `json:"kind"`
	UserID   uint64 `json:"user_id,omitempty"`
	Username string `json:"username,omitempty"`
	// PlatformAdmin reports that the user administers the platform.
	PlatformAdmin bool `json:"platform_admin,omitempty"`
	// LocalAdmin reports the break-glass administrator: only ever true for
	// a platform administrator signed in with a session.
	LocalAdmin bool `json:"local_admin,omitempty"`
	// Roles are the user's roles, sorted; a user has at least one.
	Roles []string `json:"roles,omitempty"`
	// ScopeProjects limits the data the user may read to these projects,
	// sorted; empty means no project limit.
	ScopeProjects []string `json:"scope_projects,omitempty"`
	// APIKeyID is the platform token's id; set exactly for kind token.
	APIKeyID uint64 `json:"api_key_id,omitempty"`
	// RequestID is the server's id of the request, for correlating logs.
	RequestID string `json:"request_id"`
	// RouteAction is the action of the route the server matched,
	// "<package id>.<verb>"; RequirePrincipal in the SDK refuses a request
	// whose action differs from the route the plugin matched.
	RouteAction string `json:"route_action"`
	// Route is the pattern of the route the server matched, as declared
	// in the manifest.
	Route string `json:"route"`
	// Exp is when the principal stops being valid.
	Exp Time `json:"exp"`
}

// DecodePrincipal decodes and validates a Principal's JSON.
func DecodePrincipal(data []byte) (*Principal, error) {
	if len(data) > MaxPrincipalBytes {
		return nil, malformed(objectPrincipal, &fieldError{rule: RuleRange})
	}
	var p Principal
	if err := decodeStrict(objectPrincipal, data, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the principal's rules.
func (p *Principal) Validate() error {
	if f := p.validate(); f != nil {
		return wireInvalid(objectPrincipal, f)
	}
	return nil
}

func (p *Principal) validate() *fieldError {
	if f := first(
		oneOf("kind", p.Kind, PrincipalSession, PrincipalToken, PrincipalPublic),
		text("request_id", p.RequestID, maxRequestID),
		checkRouteAction("route_action", p.RouteAction),
		checkRoute("route", p.Route),
		requiredTime("exp", p.Exp),
	); f != nil {
		return f
	}
	relation := func(field string) *fieldError { return &fieldError{field: field, rule: RuleRelation} }
	if p.Kind == PrincipalPublic {
		switch {
		case p.UserID != 0:
			return relation("user_id")
		case p.Username != "":
			return relation("username")
		case p.PlatformAdmin:
			return relation("platform_admin")
		case p.LocalAdmin:
			return relation("local_admin")
		case len(p.Roles) > 0:
			return relation("roles")
		case len(p.ScopeProjects) > 0:
			return relation("scope_projects")
		case p.APIKeyID != 0:
			return relation("api_key_id")
		}
		return nil
	}
	if f := first(
		inRange("user_id", int64(min(p.UserID, maxSafeInteger+1)), 1, maxSafeInteger),
		text("username", p.Username, maxUsername),
		checkSortedSet("scope_projects", p.ScopeProjects, projectRefPattern),
	); f != nil {
		return f
	}
	if len(p.Roles) == 0 {
		return &fieldError{field: "roles", rule: RuleRequired}
	}
	if f := checkSortedSet("roles", p.Roles, wordPattern); f != nil {
		return f
	}
	if p.LocalAdmin && (p.Kind != PrincipalSession || !p.PlatformAdmin) {
		return relation("local_admin")
	}
	if p.Kind == PrincipalToken {
		return inRange("api_key_id", int64(min(p.APIKeyID, maxSafeInteger+1)), 1, maxSafeInteger)
	}
	if p.APIKeyID != 0 {
		return relation("api_key_id")
	}
	return nil
}

// checkRoute checks a route pattern as the manifest does.
func checkRoute(path, pattern string) *fieldError {
	if f := matches(path, pattern, routePatternPattern); f != nil {
		return f
	}
	return checkRouteSegments(path, pattern)
}

// checkRouteAction checks "<package id>.<verb>".
func checkRouteAction(path, action string) *fieldError {
	if f := matches(path, action, verbPattern); f != nil {
		return f
	}
	id, verb, ok := strings.Cut(action, ".")
	if !ok || verb == "" || !packageIDPattern.MatchString(id) {
		return &fieldError{field: path, rule: RuleGrammar}
	}
	return nil
}

// PrincipalExpiry is the expiry of a principal the server signs at now for
// a route with timeoutS (0 is DefaultRouteTimeoutS): the route's timeout
// plus PrincipalExpirySlackS, rounded up to a whole second. A stream route's
// timeout bounds the wait for its first byte, so the same rule holds.
func PrincipalExpiry(now time.Time, timeoutS int) Time {
	if timeoutS <= 0 {
		timeoutS = DefaultRouteTimeoutS
	}
	exp := now.UTC().Add(time.Duration(timeoutS+PrincipalExpirySlackS) * time.Second)
	if t := exp.Truncate(time.Second); t.Before(exp) {
		exp = t.Add(time.Second)
	}
	return NewTime(exp)
}

// Feature access levels a route's request can carry: a hidden feature's
// route answers 404 and never reaches the plugin.
var featureAccessLevels = []string{AccessFull, AccessReadOnly, AccessLocked}

const objectFeatureAccess = "feature_access"

// FeatureAccess maps each feature key a matched route declares to its
// access level now: full, read_only or locked.
type FeatureAccess map[string]string

// String is the header value: "<key>=<level>" entries sorted by key and
// joined by ",", without spaces; "" when there are none. It assumes f is
// valid (Validate).
func (f FeatureAccess) String() string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k + "=" + f[k])
	}
	return b.String()
}

// Validate checks every key and level.
func (f FeatureAccess) Validate() error {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !featureKeyPattern.MatchString(k) {
			return wireInvalid(objectFeatureAccess, &fieldError{field: k, rule: RuleGrammar})
		}
		if ferr := oneOf(k, f[k], featureAccessLevels...); ferr != nil {
			return wireInvalid(objectFeatureAccess, ferr)
		}
	}
	return nil
}

// ParseFeatureAccess reads a HeaderFeatureAccess value. It accepts only the
// one spelling String writes: sorted keys, no duplicates, no spaces.
func ParseFeatureAccess(s string) (FeatureAccess, error) {
	out := FeatureAccess{}
	if s == "" {
		return out, nil
	}
	prev := ""
	for i, entry := range strings.Split(s, ",") {
		at := strconv.Itoa(i)
		k, level, ok := strings.Cut(entry, "=")
		switch {
		case !ok || !featureKeyPattern.MatchString(k):
			return nil, wireInvalid(objectFeatureAccess, &fieldError{field: at, rule: RuleGrammar})
		case i > 0 && k == prev:
			return nil, wireInvalid(objectFeatureAccess, &fieldError{field: at, rule: RuleDuplicate})
		case i > 0 && k < prev:
			return nil, wireInvalid(objectFeatureAccess, &fieldError{field: at, rule: RuleOrder})
		}
		if ferr := oneOf(at, level, featureAccessLevels...); ferr != nil {
			return nil, wireInvalid(objectFeatureAccess, ferr)
		}
		out[k], prev = level, k
	}
	return out, nil
}

// Failures of VerifyPrincipal besides a malformed principal (an *Error).
// They are the plugin's own: the server never sees them.
var (
	ErrPrincipalMissing = errors.New("extapi: the request carries no principal")
	ErrPrincipalMAC     = errors.New("extapi: the principal's MAC does not verify")
	ErrPrincipalExpired = errors.New("extapi: the principal has expired")
	// ErrPrincipalTooLong: the expiry lies further ahead than the longest
	// route timeout allows, which the server never signs.
	ErrPrincipalTooLong = errors.New("extapi: the principal's expiry is beyond any route's timeout")
)

var b64url = base64.RawURLEncoding

// SignPrincipal returns the HeaderPrincipal and HeaderPrincipalMac values
// for p and the HeaderFeatureAccess value featureAccess, under the
// per-spawn principal key: the principal's canonical JSON base64url-encoded,
// and base64url(HMAC-SHA256(key, principal header + "\n" + featureAccess)).
func SignPrincipal(key []byte, p *Principal, featureAccess string) (header, mac string, err error) {
	if len(key) != PrincipalKeyBytes {
		return "", "", fmt.Errorf("extapi: a principal key has %d bytes, not %d", PrincipalKeyBytes, len(key))
	}
	if err := p.Validate(); err != nil {
		return "", "", err
	}
	if _, err := ParseFeatureAccess(featureAccess); err != nil {
		return "", "", err
	}
	raw, err := Marshal(p)
	if err != nil {
		return "", "", err
	}
	if len(raw) > MaxPrincipalBytes {
		return "", "", malformed(objectPrincipal, &fieldError{rule: RuleRange})
	}
	header = b64url.EncodeToString(raw)
	return header, b64url.EncodeToString(principalMAC(key, header, featureAccess)), nil
}

func principalMAC(key []byte, header, featureAccess string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(header))
	m.Write([]byte{'\n'})
	m.Write([]byte(featureAccess))
	return m.Sum(nil)
}

// VerifyPrincipal checks the three header values of a request under key
// and returns the principal and the feature access they carry. It checks
// the MAC first, in constant time, and only then decodes: strictly, as
// canonical JSON with no unknown field. A principal whose Exp is before now,
// or further ahead of now than the longest route timeout, is refused.
func VerifyPrincipal(key []byte, header, mac, featureAccess string, now time.Time) (*Principal, FeatureAccess, error) {
	if header == "" || mac == "" {
		return nil, nil, ErrPrincipalMissing
	}
	if len(key) != PrincipalKeyBytes {
		return nil, nil, ErrPrincipalMAC
	}
	got, err := b64url.DecodeString(mac)
	if err != nil || !hmac.Equal(got, principalMAC(key, header, featureAccess)) {
		return nil, nil, ErrPrincipalMAC
	}
	if len(header) > b64url.EncodedLen(MaxPrincipalBytes) {
		return nil, nil, malformed(objectPrincipal, &fieldError{rule: RuleRange})
	}
	raw, err := b64url.DecodeString(header)
	if err != nil {
		return nil, nil, malformed(objectPrincipal, &fieldError{rule: RuleSyntax})
	}
	p, err := DecodePrincipal(raw)
	if err != nil {
		return nil, nil, err
	}
	if !IsCanonical(raw) {
		return nil, nil, newError(ErrorWireNotCanonical, map[string]any{"object": objectPrincipal},
			"%s: not canonical JSON", objectPrincipal)
	}
	access, err := ParseFeatureAccess(featureAccess)
	if err != nil {
		return nil, nil, err
	}
	if now.After(p.Exp.Time) {
		return nil, nil, ErrPrincipalExpired
	}
	if p.Exp.After(PrincipalExpiry(now, MaxRouteTimeoutS).Time) {
		return nil, nil, ErrPrincipalTooLong
	}
	return p, access, nil
}
