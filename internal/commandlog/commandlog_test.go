package commandlog

import (
	"strings"
	"testing"
)

func TestRedactHidesCredentialArguments(t *testing.T) {
	command := "TOKEN=env-secret curl --token 'flag-secret' --password=pass-secret -H 'Authorization: Bearer bearer-secret' 'https://example.test/path?access_key=query-secret&ok=1'"
	got := Redact(command)
	for _, secret := range []string{"env-secret", "flag-secret", "pass-secret", "bearer-secret", "query-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted command contains %q: %q", secret, got)
		}
	}
	if strings.Count(got, "<redacted>") != 5 {
		t.Fatalf("redacted command=%q", got)
	}
}
