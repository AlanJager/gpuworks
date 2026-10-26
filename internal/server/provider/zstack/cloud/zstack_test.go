package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/pkg/localization"
)

func TestRegisteredCapabilities(t *testing.T) {
	drv, ok := provider.Get("zstack")
	if !ok {
		t.Fatal("zstack driver not registered")
	}
	caps := drv.Capabilities()
	if !caps.CanListInstances || caps.CanPowerControl || !caps.CanInstallAgent || caps.CanCreate {
		t.Fatalf("capabilities should expose listing and agent installation: %+v", caps)
	}
	if _, err := drv.ListInstances(context.Background(), provider.Credentials{}); err == nil {
		t.Fatal("ListInstances should reject missing credentials")
	}
}

func TestListInstancesReturnsHostsWithAndWithoutGPUs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validTestAccessKey(r) {
			t.Fatal("missing Cloud authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/hosts":
			_, _ = w.Write([]byte(`{"inventories":[
				{"uuid":"host-gpu","name":"GPU host","managementIp":"192.0.2.10","hypervisorType":"KVM","state":"Enabled","status":"Connected"},
				{"uuid":"host-empty","name":"Empty host","managementIp":"192.0.2.11","hypervisorType":"KVM","state":"Enabled","status":"Connected"}
			]}`))
		case "/v1/gpu-device/gpu-devices":
			_, _ = w.Write([]byte(`{"inventories":[
				{"uuid":"gpu-used","hostUuid":"host-gpu","state":"Enabled","status":"Ready","allocateStatus":"Used"},
				{"uuid":"gpu-free","hostUuid":"host-gpu","state":"Enabled","status":"Ready","allocateStatus":"Available"}
			]}`))
		case "/v1/resource-configurations/host-gpu/kvm/reservedMemory", "/v1/resource-configurations/host-empty/kvm/reservedMemory":
			_, _ = w.Write([]byte(`{"value":"1G"}`))
		case "/v1/resource-configurations/host-gpu/kvm/reservedCpu", "/v1/resource-configurations/host-empty/kvm/reservedCpu":
			_, _ = w.Write([]byte(`{"value":"4"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	instances, err := New().ListInstances(context.Background(), provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 2 {
		t.Fatalf("instances = %+v, want GPU and empty hosts", instances)
	}
	empty := instances[1]
	if empty.ExternalID != "host-empty" || empty.GPUCount != 0 || empty.PublicIP != "192.0.2.11" || empty.SSHPort != 22 {
		t.Fatalf("host without GPUs is missing or not connectable: %+v", empty)
	}
	instance := instances[0]
	if instance.ExternalID != "host-gpu" || instance.GPUCount != 2 {
		t.Fatalf("instance identity/resources = %+v", instance)
	}
	if instance.Power != provider.PowerRunning || instance.PublicIP != "192.0.2.10" || instance.SSHPort != 22 {
		t.Fatalf("instance connectivity/state = %+v", instance)
	}
}

func TestValidateCredentialsRequiresEndpointAndAccessKey(t *testing.T) {
	drv := New()
	if err := drv.ValidateCredentials(context.Background(), provider.Credentials{PublicKey: "test-access-key", PrivateKey: "test-access-secret"}); err == nil {
		t.Fatal("ValidateCredentials should require endpoint")
	}
	for _, creds := range []provider.Credentials{
		{Endpoint: "https://zstack.example", APIKey: "old-session"},
		{Endpoint: "https://zstack.example", PublicKey: "test-access-key"},
		{Endpoint: "https://zstack.example", PrivateKey: "test-access-secret"},
		{Endpoint: "https://zstack.example", PublicKey: "  ", PrivateKey: "test-access-secret"},
		{Endpoint: "https://zstack.example", PublicKey: "test-access-key", PrivateKey: "  "},
	} {
		if err := drv.ValidateCredentials(t.Context(), creds); err == nil || !strings.Contains(err.Error(), "AccessKey") {
			t.Fatalf("incomplete AccessKey must be rejected before making a request: %v", err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/hosts" || !validTestAccessKey(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"inventories":[]}`))
	}))
	defer server.Close()
	if err := drv.ValidateCredentials(context.Background(), provider.Credentials{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "test-access-secret"}); err != nil {
		t.Fatalf("ValidateCredentials: %v", err)
	}
	for _, creds := range []provider.Credentials{
		{Endpoint: server.URL, PublicKey: "test-access-key", PrivateKey: "wrong-secret"},
		{Endpoint: server.URL, PublicKey: "wrong-key", PrivateKey: "test-access-secret"},
	} {
		err := drv.ValidateCredentials(t.Context(), creds)
		var apiErr *apiclient.APIError
		if validationErrorCode(t, err) != messages.ErrorZStackAuthRejected || !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden {
			t.Fatalf("invalid AccessKey must be rejected by Cloud: %v", err)
		}
	}
}

func TestKindInfoSchema(t *testing.T) {
	var info *provider.KindInfo
	for _, candidate := range provider.KindInfos() {
		if candidate.Kind == "zstack" {
			info = &candidate
			break
		}
	}
	if info == nil {
		t.Fatal("zstack kind info missing")
	}
	if info.Label[localization.English] != "ZStack Cloud" || info.Label[localization.Chinese] == "" {
		t.Fatalf("Label = %#v", info.Label)
	}
	if !hasField(info.CredentialFields, "endpoint", false, true) {
		t.Fatalf("endpoint field missing: %+v", info.CredentialFields)
	}
	if len(info.CredentialFields) != 3 || !hasField(info.CredentialFields, "public_key", false, true) ||
		!hasField(info.CredentialFields, "private_key", true, true) {
		t.Fatalf("AccessKey ID/Secret fields missing: %+v", info.CredentialFields)
	}
	for _, field := range info.CredentialFields[1:] {
		for _, locale := range []string{localization.English, localization.Chinese} {
			if !strings.Contains(field.Label[locale], "AccessKey") {
				t.Fatalf("credential label must identify AccessKey: %+v", field.Label)
			}
		}
	}
}

func hasField(fields []provider.CredentialField, name string, secret, required bool) bool {
	for _, field := range fields {
		if field.Name == name {
			return field.Secret == secret && field.Required == required
		}
	}
	return false
}
