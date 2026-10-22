package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type launchFactsHarness struct {
	store  *store.Store
	router http.Handler
	token  string
}

func newLaunchFactsHarness(t *testing.T, name string) *launchFactsHarness {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner(name+"-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &launchFactsHarness{store: st, router: Router(Deps{AuthSigner: signer, Store: st}), token: token}
}

func (h *launchFactsHarness) request(method, path string, authenticated bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, req)
	return recorder
}

func TestInstanceLaunchFactsUnknownProjectionHasExactWireShape(t *testing.T) {
	h := newLaunchFactsHarness(t, "launch-facts-unknown")
	if _, err := h.store.CreateInstance(&store.Instance{UUID: "unknown-facts", DeploymentID: 999, State: proto.InstPending}); err != nil {
		t.Fatal(err)
	}
	recorder := h.request(http.MethodGet, "/api/v1/instances/unknown-facts/launch-facts", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	want := `{"intent":{"status":"unknown"},"resolved":{"status":"unknown"},"runtime_observation_relation":{"status":"unknown","reason":"not_collected"},"runtime_observation":{"runtime":"","container":{"status":"unknown","reason":"not_collected","entrypoint":[],"cmd":[],"args":[],"env":{},"mounts":[],"devices":[]},"image":{"status":"unknown","reason":"not_collected","entrypoint":[],"cmd":[],"env":{},"volumes":[]},"process":{"status":"unknown","reason":"not_collected"}}}`
	if got := strings.TrimSpace(recorder.Body.String()); got != want {
		t.Fatalf("wire response\n got: %s\nwant: %s", got, want)
	}
}

func TestInstanceLaunchFactsRedactsIntentResolvedAndObservation(t *testing.T) {
	h := newLaunchFactsHarness(t, "launch-facts-redaction")
	deployment, err := h.store.CreateDeployment(&store.Deployment{
		Name: "safe-name", Engine: "vllm", Image: "https://user:image-secret@example.test/model", ServedName: "served", State: store.DeployActive,
		Env: `{"HF_TOKEN":"intent-key-secret","CONFIG":"{\"api_token\":\"intent-json-secret\"}","LOG_LEVEL":"info"}`, Replicas: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	contract, _ := json.Marshal(map[string]any{"auth": map[string]any{"api_key": "contract-secret"}, "path": "/v1/chat"})
	desired := proto.InstanceDesiredState{UUID: "redacted-facts", Image: "example.test/model:tag", ResolvedLaunch: proto.ResolvedLaunch{
		Argv:           []string{"/bin/sh", "-c", "cat <<EOF\n{\"api_token\":\"argv-secret\"}\nEOF"},
		PreStartScript: "export HF_TOKEN=prestart-secret\nrun", Env: map[string]string{"CONFIG": `{"password":"resolved-env-secret"}`},
		DockerArgs: []string{"--api-key", "docker-secret"}, APIContract: contract,
		PlacementVariables: map[string]any{"nested": map[string]any{"access_key": "placement-secret"}},
		RuntimeAssets:      []proto.RuntimeAsset{{File: "serve.py", Content: []byte("asset-secret")}}, ModelMount: "/model",
	}}
	desiredRaw, _ := json.Marshal(desired)
	observation := proto.InstanceRuntimeObservation{
		Runtime: "docker",
		Container: proto.RuntimeContainerObservation{Status: proto.ObservationObserved, ContainerID: "immutable-container", ImageID: "sha256:immutable-image",
			Entrypoint: []string{}, Cmd: []string{}, Args: []string{"serve", "--token", "observation-argv-secret"},
			Env: map[string]string{"CONFIG": `{"api_token":"observation-env-secret"}`}, Mounts: []proto.RuntimeMountObservation{}, Devices: []string{}},
		Image:   proto.RuntimeImageObservation{Status: proto.ObservationObserved, ImageID: "sha256:immutable-image", Entrypoint: []string{}, Cmd: []string{}, Env: map[string]string{"HF_TOKEN": "image-env-secret"}, Volumes: []string{}},
		Process: proto.RuntimeProcessObservation{Status: proto.ObservationUnknown, Reason: "not_collected"},
	}
	observationRaw, _ := json.Marshal(observation)
	receivedAt := time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC)
	if _, err := h.store.CreateInstance(&store.Instance{
		UUID: "redacted-facts", DeploymentID: deployment.ID, State: proto.InstRunning, DesiredStateJSON: string(desiredRaw),
		RuntimeObservation: string(observationRaw), RuntimeObservationReceivedAt: &receivedAt,
	}); err != nil {
		t.Fatal(err)
	}
	recorder := h.request(http.MethodGet, "/api/v1/instances/redacted-facts/launch-facts", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, secret := range []string{"image-secret", "intent-key-secret", "intent-json-secret", "contract-secret", "argv-secret", "prestart-secret", "resolved-env-secret", "docker-secret", "placement-secret", "asset-secret", "observation-argv-secret", "observation-env-secret", "image-env-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("secret %q survived launch-facts wire: %s", secret, body)
		}
	}
	var decoded struct {
		Intent struct {
			Status string `json:"status"`
		} `json:"intent"`
		Resolved struct {
			Status string `json:"status"`
		} `json:"resolved"`
		RuntimeObservation proto.InstanceRuntimeObservation `json:"runtime_observation"`
		ReceivedAt         time.Time                        `json:"runtime_observation_received_at"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Intent.Status != "available" || decoded.Resolved.Status != "available" {
		t.Fatalf("availability intent=%q resolved=%q", decoded.Intent.Status, decoded.Resolved.Status)
	}
	if decoded.RuntimeObservation.Container.ContainerID != "immutable-container" || decoded.RuntimeObservation.Image.ImageID != "sha256:immutable-image" {
		t.Fatalf("runtime observation=%+v", decoded.RuntimeObservation)
	}
	if !decoded.ReceivedAt.Equal(receivedAt) {
		t.Fatalf("received_at=%s want=%s", decoded.ReceivedAt, receivedAt)
	}
}

func TestInstanceLaunchFactsPreservesStableUnknownReasonsAndRejectsEmptyDesired(t *testing.T) {
	h := newLaunchFactsHarness(t, "launch-facts-unknown-reasons")
	observation := proto.InstanceRuntimeObservation{
		Container: proto.RuntimeContainerObservation{Status: proto.ObservationUnknown, Reason: "permission_denied", ContainerID: "must-be-cleared"},
		Image:     proto.RuntimeImageObservation{Status: proto.ObservationUnknown, Reason: "timeout", ImageID: "must-be-cleared"},
		Process:   proto.RuntimeProcessObservation{Status: proto.ObservationUnknown, Reason: "non_local_pid", PID: 99},
	}
	raw, _ := json.Marshal(observation)
	if _, err := h.store.CreateInstance(&store.Instance{UUID: "unknown-reasons", State: proto.InstRunning, DesiredStateJSON: `{}`, RuntimeObservation: string(raw)}); err != nil {
		t.Fatal(err)
	}
	recorder := h.request(http.MethodGet, "/api/v1/instances/unknown-reasons/launch-facts", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Resolved struct {
			Status string `json:"status"`
		} `json:"resolved"`
		Runtime proto.InstanceRuntimeObservation `json:"runtime_observation"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Resolved.Status != "unknown" {
		t.Fatalf("empty desired state projected as %q", response.Resolved.Status)
	}
	if response.Runtime.Container.Reason != "permission_denied" || response.Runtime.Image.Reason != "timeout" || response.Runtime.Process.Reason != "non_local_pid" {
		t.Fatalf("unknown reasons=%+v", response.Runtime)
	}
	if response.Runtime.Container.ContainerID != "" || response.Runtime.Image.ImageID != "" || response.Runtime.Process.PID != 0 {
		t.Fatalf("unknown layer retained values: %+v", response.Runtime)
	}
	if strings.Contains(recorder.Body.String(), "must-be-cleared") {
		t.Fatalf("unknown values survived wire: %s", recorder.Body.String())
	}
	for _, test := range []struct {
		raw       string
		available bool
	}{
		{raw: `{}`},
		{raw: `null`},
		{raw: `{"uuid":"unknown-reasons","image":"image:tag"}`},
		{raw: `{"uuid":"unknown-reasons","image":"image:tag","launch":null}`},
		{raw: `{"uuid":"unknown-reasons","image":"image:tag","launch":{}}`, available: true},
	} {
		_, available := resolvedLaunchFromDesired("unknown-reasons", test.raw)
		if available != test.available {
			t.Fatalf("desired %s availability=%v, want %v", test.raw, available, test.available)
		}
	}
}

func TestInstanceLaunchFactsRouteAuthNotFoundAndMethod(t *testing.T) {
	h := newLaunchFactsHarness(t, "launch-facts-routing")
	if got := h.request(http.MethodGet, "/api/v1/instances/missing/launch-facts", false); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", got.Code, got.Body.String())
	}
	if got := h.request(http.MethodGet, "/api/v1/instances/missing/launch-facts", true); got.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d body=%s", got.Code, got.Body.String())
	}
	if got := h.request(http.MethodPost, "/api/v1/instances/missing/launch-facts", true); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported method status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestRuntimeObservationRelationDoesNotPresentStaleOrUnprovableFactsAsCurrent(t *testing.T) {
	currentID := strings.Repeat("a", 64)
	replacedID := strings.Repeat("b", 64)
	observation := proto.InstanceRuntimeObservation{Container: proto.RuntimeContainerObservation{Status: proto.ObservationObserved, ContainerID: currentID}}
	for _, test := range []struct {
		name, state, lifecycleID, status, reason string
	}{
		{name: "matching immutable identity", state: proto.InstRunning, lifecycleID: currentID[:12], status: "current"},
		{name: "replaced immutable identity", state: proto.InstRunning, lifecycleID: replacedID, status: "historical", reason: "container_replaced"},
		{name: "terminal instance", state: proto.InstStopped, lifecycleID: currentID, status: "historical", reason: "instance_terminal"},
		{name: "lookup name cannot prove identity", state: proto.InstRunning, lifecycleID: "gpuworks-instance", status: "unverified", reason: "lifecycle_identity_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := runtimeObservationRelation(test.state, test.lifecycleID, observation, true)
			if got.Status != test.status || got.Reason != test.reason {
				t.Fatalf("relation=%+v, want status=%q reason=%q", got, test.status, test.reason)
			}
		})
	}
}
