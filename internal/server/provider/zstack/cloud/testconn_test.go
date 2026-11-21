package cloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/testconn"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
)

// Wire fixtures follow Cloud 4.8.0's RestServer envelopes: the plain-text
// unmapped-route reply, the SYS.1006 authentication envelope and the SYS.1007
// cannot-bind envelope. The unsigned clock-skew probe hits "/" only.
type fixtureReply struct {
	status int
	body   string
}

func testconnCloud(t *testing.T, replies map[string]fixtureReply) provider.Credentials {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet || !validTestAccessKey(r) {
			t.Errorf("unexpected Cloud request: %s %s", r.Method, r.URL)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		reply, ok := replies[r.URL.Path]
		if !ok {
			// RestServer's unmapped-route reply: plain text, no JSON envelope.
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "no api mapping to "+r.URL.Path)
			return
		}
		if reply.status != 0 && reply.status != http.StatusOK {
			w.WriteHeader(reply.status)
		}
		fmt.Fprint(w, reply.body)
	}))
	t.Cleanup(server.Close)
	return provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}
}

func runConnectionTest(t *testing.T, creds provider.Credentials) ([]testconn.Check, testconn.Summary) {
	t.Helper()
	satisfied := func(name string) bool {
		switch name {
		case "endpoint":
			return strings.TrimSpace(creds.Endpoint) != ""
		case "public_key":
			return strings.TrimSpace(creds.PublicKey) != ""
		case "private_key":
			return strings.TrimSpace(creds.PrivateKey) != ""
		}
		return false
	}
	in := testconn.Input{
		Kind:                 "zstack",
		Endpoint:             creds.Endpoint,
		RequiredCredentials:  []string{"endpoint", "public_key", "private_key"},
		CredentialsSatisfied: satisfied,
		Vendor:               New().ConnectionChecks(creds),
	}
	var checks []testconn.Check
	// The fixture listens on loopback; the host's proxy variables are not
	// part of what these tests examine.
	runner := testconn.New()
	runner.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	runner.Getenv = func(string) string { return "" }
	summary, err := runner.Run(t.Context(), in, func(c testconn.Check) { checks = append(checks, c) })
	if err != nil {
		t.Fatal(err)
	}
	return checks, summary
}

func requireCheck(t *testing.T, checks []testconn.Check, name string, status testconn.Status, code string) testconn.Check {
	t.Helper()
	for _, c := range checks {
		if c.Name != name {
			continue
		}
		if c.Status != status {
			t.Fatalf("check %s status=%s, want %s (code %s, detail %v)", name, c.Status, status, c.Code, c.Detail)
		}
		if code != "" && c.Code != code {
			t.Fatalf("check %s code=%s, want %s", name, c.Code, code)
		}
		return c
	}
	t.Fatalf("check %s not reported", name)
	return testconn.Check{}
}

func requireNoSecretLeak(t *testing.T, checks []testconn.Check, remoteTexts ...string) {
	t.Helper()
	wire, err := json.Marshal(checks)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range append([]string{"test-access-key", "test-access-secret"}, remoteTexts...) {
		if strings.Contains(string(wire), leaked) {
			t.Fatalf("check wire output leaks %q: %s", leaked, wire)
		}
	}
}

func assertVendorSkipped(t *testing.T, checks []testconn.Check) {
	t.Helper()
	for _, name := range []string{testconn.CheckProtocolShape, testconn.CheckAPIEndpoints, testconn.CheckReadPermissions, testconn.CheckResourceSummary} {
		requireCheck(t, checks, name, testconn.StatusSkipped, "")
	}
}

const (
	testconnHosts = `{"inventories":[` +
		`{"uuid":"host1","name":"GPU host 1","managementIp":"192.0.2.11","hypervisorType":"KVM","state":"Enabled","status":"Connected"},` +
		`{"uuid":"host2","name":"GPU host 2","managementIp":"192.0.2.11","hypervisorType":"KVM","state":"Enabled","status":"Connected"}]}`
	testconnGPUDevices = `{"inventories":[` +
		`{"uuid":"gpu1","hostUuid":"host1","gpuType":"NVIDIA A100","state":"Enabled","status":"Active","allocateStatus":"Unallocated"},` +
		`{"uuid":"gpu2","hostUuid":"host2","gpuType":"NVIDIA A100","state":"Enabled","status":"Attached","allocateStatus":"Allocated","vmInstanceUuid":"vm1"}]}`
	testconnPCIDevices = `{"inventories":[` +
		`{"uuid":"gpu1","hostUuid":"host1","name":"NVIDIA A100","type":"GPU_3D_Controller","pciDeviceAddress":"0000:17:00.0","state":"Enabled","status":"Active","virtStatus":"UNVIRTUALIZABLE"},` +
		`{"uuid":"gpu2","hostUuid":"host2","name":"NVIDIA A100","type":"GPU_3D_Controller","pciDeviceAddress":"0000:18:00.0","state":"Enabled","status":"Attached","vmInstanceUuid":"vm1"}]}`
)

func testconnHealthyReplies() map[string]fixtureReply {
	replies := map[string]fixtureReply{
		"/v1/hosts":                  {body: testconnHosts},
		"/v1/gpu-device/gpu-devices": {body: testconnGPUDevices},
		"/v1/global-configurations":  {body: `{"inventories":[]}`},
	}
	for _, host := range []string{"host1", "host2"} {
		replies["/v1/resource-configurations/"+host+"/kvm/reservedMemory"] = fixtureReply{body: `{"value":"1G"}`}
		replies["/v1/resource-configurations/"+host+"/kvm/reservedCpu"] = fixtureReply{body: `{"value":"2"}`}
	}
	return replies
}

// Regression: queryRaw must join limit=1 with "&" when the path already
// carries a query — plain "?" would corrupt the last filter into
// "q=name=reservedCpu?limit=1".
func TestQueryRawAppendsLimitToExistingQuery(t *testing.T) {
	var rawQueries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !validTestAccessKey(r) {
			t.Errorf("unexpected Cloud request: %s %s", r.Method, r.URL)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		rawQueries = append(rawQueries, r.URL.RawQuery)
		fmt.Fprint(w, `{"inventories":[]}`)
	}))
	t.Cleanup(server.Close)
	client, err := apiclient.New(provider.Credentials{
		Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queryRaw(t.Context(), client, "/global-configurations?q=category=kvm&q=name=reservedCpu"); err != nil {
		t.Fatal(err)
	}
	if _, err := queryRaw(t.Context(), client, "/hosts"); err != nil {
		t.Fatal(err)
	}
	if len(rawQueries) != 2 {
		t.Fatalf("requests = %d, want 2", len(rawQueries))
	}
	if rawQueries[0] != "q=category=kvm&q=name=reservedCpu&limit=1" {
		t.Fatalf("raw query with existing filters = %q", rawQueries[0])
	}
	if rawQueries[1] != "limit=1" {
		t.Fatalf("raw query without filters = %q", rawQueries[1])
	}
}

func TestConnectionChecksModernCloud(t *testing.T) {
	checks, summary := runConnectionTest(t, testconnCloud(t, testconnHealthyReplies()))
	if !summary.OK || len(summary.Failed) != 0 {
		t.Fatalf("summary=%+v", summary)
	}
	shape := requireCheck(t, checks, testconn.CheckProtocolShape, testconn.StatusOK, "")
	if shape.Detail["shape"] != "modern" {
		t.Fatalf("protocol_shape detail=%v", shape.Detail)
	}
	requireCheck(t, checks, testconn.CheckAPIEndpoints, testconn.StatusOK, "")
	requireCheck(t, checks, testconn.CheckReadPermissions, testconn.StatusOK, "")
	resources := requireCheck(t, checks, testconn.CheckResourceSummary, testconn.StatusOK, "")
	if resources.Detail["hosts"] != 2 || resources.Detail["gpus"] != 2 || resources.Detail["gpus_free"] != 1 {
		t.Fatalf("resource_summary detail=%v", resources.Detail)
	}
	requireNoSecretLeak(t, checks)
}

func TestConnectionChecksLegacyCloud(t *testing.T) {
	replies := testconnHealthyReplies()
	delete(replies, "/v1/gpu-device/gpu-devices") // Cloud 4.8.0: no such route
	replies["/v1/pci-device/pci-devices"] = fixtureReply{body: testconnPCIDevices}
	checks, summary := runConnectionTest(t, testconnCloud(t, replies))
	if !summary.OK {
		t.Fatalf("PCI-only Cloud is supported; summary=%+v", summary)
	}
	shape := requireCheck(t, checks, testconn.CheckProtocolShape, testconn.StatusOK, "")
	if shape.Detail["shape"] != "legacy" {
		t.Fatalf("protocol_shape detail=%v", shape.Detail)
	}
	endpoints := requireCheck(t, checks, testconn.CheckAPIEndpoints, testconn.StatusOK, "")
	absent, _ := endpoints.Detail["absent"].([]string)
	if len(absent) != 1 || absent[0] != "/gpu-device/gpu-devices" {
		t.Fatalf("api_endpoints must name the missing GPU route: %v", endpoints.Detail)
	}
	resources := requireCheck(t, checks, testconn.CheckResourceSummary, testconn.StatusOK, "")
	if resources.Detail["hosts"] != 2 || resources.Detail["gpus"] != 2 || resources.Detail["gpus_free"] != 1 {
		t.Fatalf("resource_summary detail=%v", resources.Detail)
	}
}

func TestConnectionChecksIntermediateCloud(t *testing.T) {
	replies := testconnHealthyReplies()
	// Intermediate releases answer the GPU query with PCI fields and no
	// allocateStatus.
	replies["/v1/gpu-device/gpu-devices"] = fixtureReply{body: testconnPCIDevices}
	checks, summary := runConnectionTest(t, testconnCloud(t, replies))
	if !summary.OK {
		t.Fatalf("summary=%+v", summary)
	}
	shape := requireCheck(t, checks, testconn.CheckProtocolShape, testconn.StatusOK, "")
	if shape.Detail["shape"] != "intermediate" {
		t.Fatalf("protocol_shape detail=%v", shape.Detail)
	}
}

// Regression: a Cloud that rejects the AK signature with a 403 whose details
// match none of the known strings (the incident behind this feature) must
// surface as auth_request failed with ZSTACK_AUTH_REJECTED, not a generic error.
func TestConnectionChecksAuthRejectedRegression(t *testing.T) {
	const remoteDetails = "signature check failed by the gateway"
	replies := map[string]fixtureReply{
		"/v1/hosts": {status: http.StatusForbidden, body: `{"error":{"code":"SYS.1006","details":"` + remoteDetails + `"}}`},
	}
	checks, summary := runConnectionTest(t, testconnCloud(t, replies))
	if summary.OK {
		t.Fatal("summary must fail")
	}
	requireCheck(t, checks, testconn.CheckAuthRequest, testconn.StatusFailed, messages.ErrorZStackAuthRejected)
	assertVendorSkipped(t, checks)
	requireNoSecretLeak(t, checks, remoteDetails)
}

func TestConnectionChecksAuthClassification(t *testing.T) {
	for _, tc := range []struct {
		name, details, code string
	}{
		{"wrong signature", "wrong accessKey signature", messages.ErrorZStackSignatureInvalid},
		{"clock skew", "requestTimeTooSkewed", messages.ErrorZStackClockSkew},
		{"access key invalid", "access key id: test-access-key does not existed or disabled", messages.ErrorZStackAccessKeyInvalid},
		{"license required", "AccessKey authentication requires valid Enterprise license", messages.ErrorZStackLicenseRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replies := map[string]fixtureReply{
				"/v1/hosts": {status: http.StatusForbidden, body: `{"error":{"code":"SYS.1006","details":"` + tc.details + `"}}`},
			}
			checks, _ := runConnectionTest(t, testconnCloud(t, replies))
			requireCheck(t, checks, testconn.CheckAuthRequest, testconn.StatusFailed, tc.code)
			assertVendorSkipped(t, checks)
			requireNoSecretLeak(t, checks, tc.details)
		})
	}
}

func TestConnectionChecksResourceConfigUnbound(t *testing.T) {
	replies := testconnHealthyReplies()
	// ResourceConfigApiInterceptor's cannot-bind diagnostic (Cloud 4.8.0).
	unbound := fixtureReply{status: http.StatusBadRequest,
		body: `{"error":{"code":"SYS.1007","description":"Invalid argument","details":"global config[category:kvm, name:kvm.reservedCpu] cannot bind resource"}}`}
	replies["/v1/resource-configurations/host1/kvm/reservedCpu"] = unbound
	replies["/v1/resource-configurations/host2/kvm/reservedCpu"] = unbound
	replies["/v1/global-configurations"] = fixtureReply{body: `{"inventories":[{"category":"kvm","name":"reservedCpu","value":"3"}]}`}
	checks, summary := runConnectionTest(t, testconnCloud(t, replies))
	if !summary.OK {
		t.Fatalf("an unbound CPU reservation config must not fail the run: summary=%+v", summary)
	}
	shape := requireCheck(t, checks, testconn.CheckProtocolShape, testconn.StatusOK, "")
	if shape.Detail["cpu_reservation_config"] != "unbound" {
		t.Fatalf("protocol_shape detail=%v", shape.Detail)
	}
}

func TestConnectionChecksGPURoutesMissing(t *testing.T) {
	replies := testconnHealthyReplies()
	delete(replies, "/v1/gpu-device/gpu-devices")
	checks, summary := runConnectionTest(t, testconnCloud(t, replies))
	if summary.OK {
		t.Fatal("a Cloud with no GPU query route is not usable")
	}
	shape := requireCheck(t, checks, testconn.CheckProtocolShape, testconn.StatusFailed, messages.ErrorZStackRequestFailed)
	if shape.Detail["shape"] != "unsupported" {
		t.Fatalf("protocol_shape detail=%v", shape.Detail)
	}
	endpoints := requireCheck(t, checks, testconn.CheckAPIEndpoints, testconn.StatusFailed, messages.ErrorZStackRequestFailed)
	missing, _ := endpoints.Detail["missing"].([]string)
	if fmt.Sprint(missing) != "[/gpu-device/gpu-devices /pci-device/pci-devices]" {
		t.Fatalf("api_endpoints must name both missing routes: %v", endpoints.Detail)
	}
}

func TestConnectionChecksReadPermissionDenied(t *testing.T) {
	replies := testconnHealthyReplies()
	replies["/v1/gpu-device/gpu-devices"] = fixtureReply{status: http.StatusForbidden,
		body: `{"error":{"code":"SYS.1006","details":"api permission denied"}}`}
	checks, summary := runConnectionTest(t, testconnCloud(t, replies))
	if summary.OK {
		t.Fatal("summary must fail")
	}
	permissions := requireCheck(t, checks, testconn.CheckReadPermissions, testconn.StatusFailed, messages.ErrorZStackAuthRejected)
	denied, _ := permissions.Detail["denied"].([]string)
	if fmt.Sprint(denied) != "[gpus]" {
		t.Fatalf("read_permissions must name the denied resource: %v", permissions.Detail)
	}
	requireCheck(t, checks, testconn.CheckResourceSummary, testconn.StatusSkipped, "")
	requireNoSecretLeak(t, checks, "api permission denied")
}
