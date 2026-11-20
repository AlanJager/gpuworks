package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The bodies are Alibaba Cloud's own: a signature it cannot reproduce is
// answered with the string to sign and the canonical request it computed.
const (
	aliyunWrongSecretBody = `{"RequestId":"5C2B4A1E-93D0-5F0B-8C55-0D6E0F1A2B3C","Message":"Specified signature does not match our calculation. server StringToSign is [ACS3-HMAC-SHA256\n67ed07c6a1f04b7e2d3c4b5a69788796a5b4c3d2e1f00112233445566778899a], server CanonicalRequest is [POST\n/\n\nhost:ecs.aliyuncs.com\nx-acs-action:DescribeRegions\nx-acs-content-sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\nx-acs-date:2026-11-19T14:38:00Z\nx-acs-signature-nonce:0123456789abcdef\nx-acs-version:2014-05-26\n\nhost;x-acs-action;x-acs-content-sha256;x-acs-date;x-acs-signature-nonce;x-acs-version\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855]","Recommend":"https://api.aliyun.com/troubleshoot?q=IncompleteSignature&product=Ecs&requestId=5C2B4A1E-93D0-5F0B-8C55-0D6E0F1A2B3C","HostId":"ecs.aliyuncs.com","Code":"IncompleteSignature"}`
	aliyunUnknownKeyBody  = `{"RequestId":"7A1D0B52-1E0C-5B7F-9E21-4C3F2A1B0D9E","Message":"Specified access key is not found.","Recommend":"https://api.aliyun.com/troubleshoot?q=InvalidAccessKeyId.NotFound&product=Ecs&requestId=7A1D0B52-1E0C-5B7F-9E21-4C3F2A1B0D9E","HostId":"ecs.aliyuncs.com","Code":"InvalidAccessKeyId.NotFound"}`
	aliyunForbiddenBody   = `{"RequestId":"0E9D8C7B-6A5F-5E4D-8C3B-2A1F0E9D8C7B","Message":"User not authorized to operate on the specified resource, or this API doesn't support RAM.","Recommend":"https://api.aliyun.com/troubleshoot?q=Forbidden.RAM&product=Ecs&requestId=0E9D8C7B-6A5F-5E4D-8C3B-2A1F0E9D8C7B","HostId":"ecs.aliyuncs.com","Code":"Forbidden.RAM"}`
	aliyunThrottledBody   = `{"RequestId":"1B2C3D4E-5F60-5A7B-8C9D-0E1F2A3B4C5D","Message":"Request was denied due to user flow control.","Recommend":"https://api.aliyun.com/troubleshoot?q=Throttling.User&product=Ecs&requestId=1B2C3D4E-5F60-5A7B-8C9D-0E1F2A3B4C5D","HostId":"ecs.aliyuncs.com","Code":"Throttling.User"}`
)

func TestAliyunProviderValidationErrorsPreserveStorageAndWireContract(t *testing.T) {
	type refusal struct {
		status int
		body   string
	}
	var answer atomic.Pointer[refusal]
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/" || r.Header.Get("x-acs-action") != "DescribeRegions" ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "ACS3-HMAC-SHA256 Credential=LTAI-fixture-id,") {
			t.Errorf("validation lost its action or signing: %s %s %q", r.Method, r.URL.Path, r.Header.Get("x-acs-action"))
		}
		w.Header().Set("Content-Type", "application/json")
		if refused := answer.Load(); refused != nil {
			w.WriteHeader(refused.status)
			fmt.Fprint(w, refused.body)
			return
		}
		fmt.Fprint(w, `{"Regions":{"Region":[{"RegionId":"cn-shanghai","LocalName":"华东2（上海）"}]},"RequestId":"3F2E1D0C-9B8A-5766-8544-33221100FFEE"}`)
	}))
	defer remote.Close()
	st, err := store.Open(t.TempDir()+"/providers.db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := auth.NewSecretbox("provider-validation-test")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	h := apiHandlers{d: Deps{Store: st, Providers: provider.NewService(st, box)}}
	request := func(method, path, name string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"kind": "aliyun", "name": name, "endpoint": remote.URL, "public_key": "LTAI-fixture-id", "private_key": "fixture-access-key-secret"})
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		w := httptest.NewRecorder()
		if method == http.MethodPost {
			h.addProvider(w, r)
		} else {
			router := chi.NewRouter()
			router.Put("/api/v1/providers/{id}", h.updateProvider)
			router.ServeHTTP(w, r)
		}
		return w
	}
	assertWire := func(w *httptest.ResponseRecorder, want string) {
		t.Helper()
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
		if got := strings.TrimSpace(w.Body.String()); got != want {
			t.Fatalf("wire error:\n got %s\nwant %s", got, want)
		}
		for _, raw := range []string{"StringToSign", "CanonicalRequest", "x-acs-", "RequestId", "IncompleteSignature", "InvalidAccessKeyId", "Forbidden.RAM", "LTAI-fixture-id", "fixture-access-key-secret"} {
			if strings.Contains(w.Body.String(), raw) {
				t.Fatalf("Alibaba Cloud's own text %q reached the response: %s", raw, w.Body)
			}
		}
	}
	const (
		wrongSecretWire = `{"code":"ALIYUN_SIGNATURE_INVALID","error":"credential validation failed: Alibaba Cloud rejected the request signature. Check that the AccessKey ID and AccessKey Secret are correct and belong together."}`
		unknownKeyWire  = `{"code":"ALIYUN_ACCESS_KEY_INVALID","error":"credential validation failed: The Alibaba Cloud AccessKey ID does not exist or is disabled. Check the AccessKey ID and its state."}`
		forbiddenWire   = `{"code":"ALIYUN_PERMISSION_DENIED","error":"credential validation failed: Alibaba Cloud accepted the AccessKey but denied the request. Check the RAM permissions of the user, and of the role when one is set."}`
	)

	answer.Store(&refusal{http.StatusBadRequest, aliyunWrongSecretBody})
	assertWire(request(http.MethodPost, "/api/v1/providers", "cloud"), wrongSecretWire)
	// What the response withholds is in the server log, without the secret.
	logged := logs.String()
	for _, want := range []string{"aliyun_code=IncompleteSignature", "request_id=5C2B4A1E-93D0-5F0B-8C55-0D6E0F1A2B3C", "server StringToSign is [ACS3-HMAC-SHA256", "x-acs-action:DescribeRegions"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log lost %q: %s", want, logged)
		}
	}
	if strings.Contains(logged, "fixture-access-key-secret") {
		t.Fatalf("the AccessKey Secret reached the log: %s", logged)
	}
	answer.Store(&refusal{http.StatusNotFound, aliyunUnknownKeyBody})
	assertWire(request(http.MethodPost, "/api/v1/providers", "cloud"), unknownKeyWire)
	answer.Store(&refusal{http.StatusForbidden, aliyunForbiddenBody})
	assertWire(request(http.MethodPost, "/api/v1/providers", "cloud"), forbiddenWire)
	providers, err := st.ListProviders()
	if err != nil || len(providers) != 0 {
		t.Fatalf("failed addition persisted: %v %v", providers, err)
	}

	// An error that is not about the credentials keeps the generic code and
	// its own text.
	answer.Store(&refusal{http.StatusBadRequest, aliyunThrottledBody})
	w := request(http.MethodPost, "/api/v1/providers", "cloud")
	var throttled map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &throttled); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusBadRequest || len(throttled) != 2 || throttled["code"] != "BAD_REQUEST" ||
		throttled["error"] != "credential validation failed: aliyun: Throttling.User: Request was denied due to user flow control. (HTTP 400, request 1B2C3D4E-5F60-5A7B-8C9D-0E1F2A3B4C5D)" {
		t.Fatalf("throttling wire changed: %d %s", w.Code, w.Body)
	}

	answer.Store(nil)
	w = request(http.MethodPost, "/api/v1/providers", "cloud")
	if w.Code != http.StatusCreated {
		t.Fatalf("valid account rejected: %d %s", w.Code, w.Body)
	}
	providers, err = st.ListProviders()
	if err != nil || len(providers) != 1 {
		t.Fatalf("successful addition not persisted: %v", err)
	}
	before := providers[0]
	answer.Store(&refusal{http.StatusBadRequest, aliyunWrongSecretBody})
	assertWire(request(http.MethodPut, fmt.Sprintf("/api/v1/providers/%d", before.ID), "changed"), wrongSecretWire)
	after, err := st.GetProvider(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name || after.CredsCipher != before.CredsCipher {
		t.Fatal("failed edit changed the saved account")
	}
}
