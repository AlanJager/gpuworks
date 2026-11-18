package pluginbridge

import (
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The principal names who calls as the server authenticated it: a token
// or a member never carries the break-glass flag, a public caller no user,
// and a request id is clipped or replaced so it always fits.
func TestPrincipalForCallers(t *testing.T) {
	r := &Route{Plugin: "acme_backup", Decl: extapi.RouteDecl{Method: "GET", Pattern: "/jobs/{id}", Action: "acme_backup.jobs_get", Access: extapi.RouteMember, Feature: "acme.backup"}}
	cases := []struct {
		name   string
		c      Caller
		check  func(*extapi.Principal) bool
		access string
	}{
		{"admin token", Caller{Kind: extapi.PrincipalToken, UserID: 1, Username: "ada", Role: "admin", LocalAdmin: true, APIKeyID: 9, RequestID: "a", Access: extapi.AccessFull},
			func(p *extapi.Principal) bool { return p.PlatformAdmin && !p.LocalAdmin && p.APIKeyID == 9 }, "acme.backup=full"},
		{"member session", Caller{Kind: extapi.PrincipalSession, UserID: 2, Username: "bo", Role: "member", LocalAdmin: true, APIKeyID: 9, RequestID: "a", Access: extapi.AccessReadOnly},
			func(p *extapi.Principal) bool {
				return !p.PlatformAdmin && !p.LocalAdmin && p.APIKeyID == 0 && p.Roles[0] == "member"
			}, "acme.backup=read_only"},
		{"public", Caller{Kind: extapi.PrincipalPublic, UserID: 3, Username: "x", Role: "admin", RequestID: "a", Access: extapi.AccessFull},
			func(p *extapi.Principal) bool {
				return p.UserID == 0 && p.Username == "" && len(p.Roles) == 0 && !p.PlatformAdmin
			}, "acme.backup=full"},
	}
	for _, c := range cases {
		p, access := principalFor(r, c.c)
		p.Exp = extapi.PrincipalExpiry(time.Now(), 0)
		if err := p.Validate(); err != nil || !c.check(p) || access != c.access || p.RouteAction != "acme_backup.jobs_get" || p.Route != "/jobs/{id}" {
			t.Errorf("%s: principal %+v (%v), access %q", c.name, p, err, access)
		}
	}
	ungated := &Route{Decl: extapi.RouteDecl{Pattern: "/", Action: "acme_backup.home", Ungated: true}}
	if _, access := principalFor(ungated, Caller{Kind: extapi.PrincipalPublic, Access: extapi.AccessFull}); access != "" {
		t.Fatalf("an ungated route's access = %q", access)
	}
	for in, want := range map[string]string{"req-1": "req-1", strings.Repeat("x", 300): strings.Repeat("x", 128)} {
		if got := requestID(in); got != want {
			t.Errorf("requestID(%.10q) = %.10q", in, got)
		}
	}
	for _, in := range []string{"", "has space", "nul\x00", "é"} {
		if got := requestID(in); len(got) != 32 || got == in {
			t.Errorf("requestID(%q) = %q, want a fresh id", in, got)
		}
	}
}
