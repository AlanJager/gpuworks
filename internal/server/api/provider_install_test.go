package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInstallAgentOnInstanceContinuesAfterPrecheckError(t *testing.T) {
	drv := &apiPrecheckErrorProviderDriver{}
	provider.Register(drv)
	st, err := store.Open("file:api_provider_install_precheck_error_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-install-precheck-error")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-precheck-error-provider", "install", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{
		"external_id":"inst-1",
		"server_url":"https://proxy.example/control",
		"password":"pw"
	}`)))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
	rec := httptest.NewRecorder()

	h.installAgentOnInstance(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if drv.req.ServerURL != "https://proxy.example/control" {
		t.Fatalf("install should proceed after precheck error, request = %+v", drv.req)
	}
	var body struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
		InstallDetails []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"install_details"`
		PrecheckDetails []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"precheck_details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Checks) < 3 || body.Checks[0].Name != "precheck" || body.Checks[0].Status != "error" {
		t.Fatalf("precheck error should be reported without blocking install: %+v", body.Checks)
	}
	// The failed item is behind the summary row too, with its reason.
	// Preparing the platform key came first, then the SSH dial that failed.
	if n := len(body.PrecheckDetails); n < 2 || body.PrecheckDetails[0].Name != "Platform key (provider)" ||
		body.PrecheckDetails[n-1].Name != "SSH" || body.PrecheckDetails[n-1].Status != "error" || body.PrecheckDetails[n-1].Detail == "" {
		t.Fatalf("precheck_details = %+v", body.PrecheckDetails)
	}
	if !strings.Contains(body.Checks[0].Detail, "SSH: ssh dial") || !strings.Contains(body.Checks[0].Detail, "Platform key (provider)") {
		t.Fatalf("precheck summary should name the failing items: %q", body.Checks[0].Detail)
	}
	if body.Checks[1].Name != "install agent" || body.Checks[1].Status != "warn" {
		t.Fatalf("install result missing after precheck error: %+v", body.Checks)
	}
	// The steps behind the summary are returned too: here the server has no
	// platform key, which the operator must be able to see.
	if len(body.InstallDetails) == 0 || body.InstallDetails[0].Name != "Platform key (provider)" || body.InstallDetails[0].Status != "warn" {
		t.Fatalf("install_details = %+v", body.InstallDetails)
	}
}

func TestInstallAgentOnInstanceRejectsInvalidServerURLBeforeCreatingNode(t *testing.T) {
	drv := &apiInstallProviderDriver{}
	provider.Register(drv)
	st, err := store.Open("file:api_provider_install_invalid_url_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-install-invalid-url")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-install-provider", "install", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{
		"external_id":"inst-1",
		"server_url":"https://control.example?unsafe=1"
	}`)))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
	rec := httptest.NewRecorder()
	h.installAgentOnInstance(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("invalid server URL left pending nodes: %+v", nodes)
	}
}

// The one-line precheck row names only what is not ok; the rest would bury the
// warning among a dozen passing items (and, with the install directory's mount
// options, run to thousands of characters).
func TestSummarizePrecheckNamesOnlyTheItemsThatAreNotOK(t *testing.T) {
	ok := func(name string) provision.CheckResult {
		return provision.CheckResult{Name: name, Status: "ok", Detail: name + " is fine, details we do not want in the summary"}
	}
	for _, tc := range []struct {
		name   string
		checks []provision.CheckResult
		want   string
		status string
	}{
		{"no checks", nil, "provider precheck completed", "ok"},
		{"all ok", []provision.CheckResult{ok("a"), ok("b"), ok("c")}, "all 3 checks passed", "ok"},
		{"one warning among ok items",
			[]provision.CheckResult{ok("a"), {Name: "Install directory free space", Status: "warn", Detail: "20.0 GiB free of 80.0 GiB"}, ok("c")},
			"Install directory free space: 20.0 GiB free of 80.0 GiB", "warn"},
		{"warning and error keep their order, an item without detail is still named",
			[]provision.CheckResult{{Name: "FUSE", Status: "warn"}, ok("b"), {Name: "SSH", Status: "error", Detail: "connection refused"}},
			"FUSE; SSH: connection refused", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizePrecheck(tc.checks); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
			if got := summarizePrecheckStatus(tc.checks); got != tc.status {
				t.Fatalf("status = %q, want %q", got, tc.status)
			}
		})
	}
}

func TestAgentDialInstallCheckReportsSessionState(t *testing.T) {
	ctx := context.Background()
	if got := agentDialInstallCheck(ctx, "node-1", func(uuid string) bool { return uuid == "node-1" }); got.Status != "ok" || got.Detail != "agent session established" {
		t.Fatalf("online check = %+v", got)
	}
	if got := agentDialInstallCheck(ctx, "node-1", nil); got.Status != "warn" {
		t.Fatalf("missing hub check = %+v", got)
	}
	if got := agentDialInstallCheck(ctx, "", func(string) bool { return true }); got.Status != "warn" {
		t.Fatalf("missing node UUID check = %+v", got)
	}
	online := make(chan struct{})
	go func() {
		time.Sleep(120 * time.Millisecond)
		close(online)
	}()
	if got := agentDialInstallCheck(ctx, "node-2", func(string) bool {
		select {
		case <-online:
			return true
		default:
			return false
		}
	}); got.Status != "ok" {
		t.Fatalf("delayed session check = %+v", got)
	}
}

type apiInstallFailingProviderDriver struct {
	apiInstallProviderDriver
}

func (d *apiInstallFailingProviderDriver) Name() string { return "api-install-failing-provider" }

func (d *apiInstallFailingProviderDriver) InstallAgentWithReport(context.Context, provider.Credentials, provider.InstallRequest) (provider.InstallReport, error) {
	return provider.InstallReport{}, errors.New("ssh: unable to authenticate; the target has not authorized the platform key")
}

// A failed install answers 424 with the reason in the body, never 502: CDNs
// and tunnels (Cloudflare) replace 502 bodies with their own error page.
func TestInstallAgentOnInstanceFailureKeepsReasonOutOfGatewayStatus(t *testing.T) {
	provider.Register(&apiInstallFailingProviderDriver{})
	st, err := store.Open("file:api_provider_install_failure_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	box, err := auth.NewSecretbox("api-provider-install-failure")
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), "api-install-failing-provider", "install", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, Providers: svc}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", bytes.NewReader([]byte(`{
		"external_id":"inst-1",
		"server_url":"https://control.example",
		"connect_mode":"agent_dial",
		"ssh_connection_id":"auto"
	}`)))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(p.ID)}))
	rec := httptest.NewRecorder()

	h.installAgentOnInstance(rec, req)
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"`) || !strings.Contains(rec.Body.String(), "has not authorized the platform key") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func providerInstallHandlers(t *testing.T, drv provider.Driver, dbName, kind string) (*apiHandlers, *store.Store, uint) {
	t.Helper()
	provider.Register(drv)
	st, err := store.Open("file:"+dbName+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox(dbName)
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), kind, "install", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st, Providers: svc}}, st, p.ID
}

func postProviderInstall(h *apiHandlers, providerID uint, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/1/install", strings.NewReader(body))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(providerID)}))
	rec := httptest.NewRecorder()
	h.installAgentOnInstance(rec, req)
	return rec
}

// install_dir and no_systemd on the wire reach the driver's install request.
func TestInstallAgentOnInstanceCarriesTheDirectoryAndSupervisor(t *testing.T) {
	drv := &apiPrecheckErrorProviderDriver{}
	h, _, providerID := providerInstallHandlers(t, drv, "api_provider_install_dir_test", "api-precheck-error-provider")
	rec := postProviderInstall(h, providerID, `{"external_id":"inst-1","server_url":"https://control.example","password":"pw","install_dir":"/data/gpuworks/","no_systemd":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if drv.req.InstallDir != "/data/gpuworks" || !drv.req.NoSystemd {
		t.Fatalf("driver request dir=%q no_systemd=%v", drv.req.InstallDir, drv.req.NoSystemd)
	}

	drv.req = provider.InstallRequest{}
	rec = postProviderInstall(h, providerID, `{"external_id":"inst-1","server_url":"https://control.example","password":"pw"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if drv.req.InstallDir != "" || drv.req.NoSystemd {
		t.Fatalf("defaults: driver request dir=%q no_systemd=%v", drv.req.InstallDir, drv.req.NoSystemd)
	}
}

// A cloud install directory is an absolute path other than /: refused before
// a pending node exists.
func TestInstallAgentOnInstanceRejectsAnInvalidInstallDir(t *testing.T) {
	drv := &apiInstallProviderDriver{}
	h, st, providerID := providerInstallHandlers(t, drv, "api_provider_install_bad_dir_test", "api-install-provider")
	for _, dir := range []string{"gpuworks", "~/gpuworks", "/", "//", `/data/a\nb`} {
		body := `{"external_id":"inst-1","server_url":"https://control.example","install_dir":"` + dir + `"}`
		rec := postProviderInstall(h, providerID, body)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || out["code"] != ErrorInstallDirInvalid {
			t.Fatalf("%q: status = %d body=%s", dir, rec.Code, rec.Body.String())
		}
	}
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("invalid install_dir left pending nodes: %+v", nodes)
	}
}
