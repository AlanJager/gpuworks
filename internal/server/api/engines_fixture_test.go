package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// enginesFixturePath is the parameter dictionary the deploy form renders in its
// own tests. Keeping one copy on disk is what lets a front-end test fail when
// the server stops publishing a key: the UI would otherwise only ever see a
// profile it wrote itself, and a gap like the KV tier keys missing from
// QuickFormKeys would go unnoticed on both sides.
const enginesFixturePath = "../../../ui/tests/fixtures/engines.json"

// enginesFixtureEnv regenerates the fixture instead of comparing against it.
const enginesFixtureEnv = "GPUWORKS_UPDATE_ENGINES_FIXTURE"

// enginesFixtureKeys is what the fixture holds: the EngineProfile fields
// ui/src/api.ts declares, which is everything a client can read.
//
// The projection keeps the response's own keys rather than re-encoding through a
// struct of its own: a key the endpoint renames or stops publishing then
// disappears from the fixture and fails this case, which a struct with its own
// json tags would silently paper over.
var enginesFixtureKeys = map[string]bool{
	"default_image":      true,
	"health_path":        true,
	"kv_disk_min_kernel": true,
	"metric_prefix":      true,
	"name":               true,
	"param_dict":         true,
	"param_specs":        true,
	"pd_modes":           true,
	"quick_form_keys":    true,
	"shm_bytes":          true,
	"sleep":              true,
}

// enginesFixtureServerOnlyKeys are the response fields no client reads, left out
// of the fixture so a change to them stops forcing a regeneration of it. Each is
// covered on its own: TestEnginesEndpointReturnsProfiles asserts metric_map key
// by key and operation_definitions against inferenceapi.Definitions().
//
// Together with enginesFixtureKeys this classification is exhaustive, and the
// case below fails on any key in neither. That is deliberate: a field added to
// the response has to be classified here at the moment it is added, because a
// client-visible field left out of the projection would leave the deploy form's
// tests rendering a profile without it - passing while covering nothing, which
// is the one way narrowing this fixture could lose coverage silently.
var enginesFixtureServerOnlyKeys = map[string]bool{
	"metric_map":            true,
	"metric_path":           true,
	"operation_definitions": true,
}

// enginesFixtureDictionaries are the engines whose dictionary the deploy form
// renders, and the keys it renders it from. An engine that stopped publishing
// either would leave the form with nothing to build its controls out of, which
// is the gap this fixture exists to catch; naming it here makes that failure say
// so instead of showing up as a diff in a 2000-line file.
var enginesFixtureDictionaries = []string{"vllm", "sglang"}

func TestEnginesResponseMatchesUIFixture(t *testing.T) {
	// default_image is deployment configuration rather than dictionary, and
	// another case in this package sets it globally with no way to restore it,
	// so it is pinned to the same value here and the fixture holds regardless
	// of the order the cases run in.
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	signer, err := auth.NewTokenSigner("engines-fixture-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/engines", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	Router(Deps{Store: st, Log: slog.Default(), AuthSigner: signer}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	got := projectEnginesFixture(t, rec.Body.Bytes())
	if os.Getenv(enginesFixtureEnv) != "" {
		if err := os.MkdirAll(filepath.Dir(enginesFixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(enginesFixturePath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("regenerated %s; rerun without %s", enginesFixturePath, enginesFixtureEnv)
	}
	want, err := os.ReadFile(enginesFixturePath)
	if err != nil {
		t.Fatalf("read engines fixture: %v (regenerate with %s=1 go test ./internal/server/api/ -run TestEnginesResponseMatchesUIFixture)", err, enginesFixtureEnv)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the client-visible part of /api/v1/engines drifted from %s; the deploy form's tests render that file, so review the change and regenerate with %s=1 go test ./internal/server/api/ -run TestEnginesResponseMatchesUIFixture", enginesFixturePath, enginesFixtureEnv)
	}
}

// projectEnginesFixture reduces the endpoint's own response to the keys clients
// read, as indented JSON so a one-key drift shows up as a one-line diff in
// review. The values are still the endpoint's bytes, copied verbatim. The
// fixture holds the profiles - the response's list envelope's items - which
// is what the deploy form's tests render.
func projectEnginesFixture(t *testing.T, body []byte) []byte {
	t.Helper()
	var profiles []map[string]json.RawMessage
	if err := unmarshalList(body, &profiles); err != nil {
		t.Fatalf("decode /api/v1/engines: %v (body %s)", err, body)
	}
	dictionaries := map[string]bool{}
	projected := make([]map[string]json.RawMessage, 0, len(profiles))
	for _, profile := range profiles {
		var name string
		if err := json.Unmarshal(profile["name"], &name); err != nil {
			t.Fatalf("engine profile without a name: %v", err)
		}
		kept := make(map[string]json.RawMessage, len(enginesFixtureKeys))
		for key, value := range profile {
			switch {
			case enginesFixtureKeys[key]:
				kept[key] = value
			case enginesFixtureServerOnlyKeys[key]:
			default:
				t.Fatalf("/api/v1/engines publishes %q on engine %q, which is in neither enginesFixtureKeys nor enginesFixtureServerOnlyKeys: add it to the first if a client reads it (the fixture must carry it, or the deploy form's tests render a profile without it), to the second if it is server-side wiring, and in that case say there which case covers it", key, name)
			}
		}
		if len(kept["param_specs"]) > 0 && len(kept["quick_form_keys"]) > 0 {
			dictionaries[name] = true
		}
		projected = append(projected, kept)
	}
	for _, engine := range enginesFixtureDictionaries {
		if !dictionaries[engine] {
			t.Fatalf("/api/v1/engines no longer publishes both param_specs and quick_form_keys for %q; the deploy form builds its controls from them", engine)
		}
	}
	// Map keys marshal in sorted order, so the file is stable across runs.
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, encoded, "", "  "); err != nil {
		t.Fatal(err)
	}
	return append(bytes.TrimRight(pretty.Bytes(), "\n"), '\n')
}
