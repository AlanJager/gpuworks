package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/version"
)

func setEdition(t *testing.T, edition string) {
	t.Helper()
	saved := version.Edition
	t.Cleanup(func() { version.Edition = saved })
	version.Edition = edition
}

// /healthz names the build's edition next to its version, so an operator or
// a supporter can tell which build answers without logging in.
func TestHealthzWireCarriesTheEdition(t *testing.T) {
	saved := version.Version
	t.Cleanup(func() { version.Version = saved })
	version.Version = "v9.9.9"
	for _, edition := range []string{"community", "oem-x"} {
		setEdition(t, edition)
		r := newAutoRig(t, 0)
		rec := httptest.NewRecorder()
		r.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		want := `{"status":"ok","version":"v9.9.9","edition":"` + edition + `","nodes":0,"sessions":0}` + "\n"
		if rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Fatalf("healthz = %d %q, want %q", rec.Code, rec.Body.String(), want)
		}
		var round dto.Health
		if err := json.Unmarshal(rec.Body.Bytes(), &round); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(round)
		if string(b)+"\n" != want {
			t.Fatalf("round-trip %s", b)
		}
	}
}

// Only the running build has a known edition; a target build named by a
// preview is another binary and carries none.
func TestServerBuildInfoNamesTheEditionOfTheRunningBuildOnly(t *testing.T) {
	setBuildInfoVersion(t, "r1", "none", "unknown", "nightly")
	setEdition(t, "oem-x")
	running := serverBuildInfo("r1", nil)
	b, err := json.Marshal(running)
	if want := `{"version":"r1","build_channel":"nightly","edition":"oem-x","kind":"development"}`; err != nil || string(b) != want {
		t.Fatalf("running build wire = %s (%v), want %s", b, err, want)
	}
	other := serverBuildInfo("r2", nil)
	b, err = json.Marshal(other)
	if want := `{"version":"r2","kind":"unknown"}`; err != nil || string(b) != want {
		t.Fatalf("target build wire = %s (%v), want %s", b, err, want)
	}
}
