package cloud

import (
	"encoding/json"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider/testconn"
)

// requireWire asserts one check exactly as a client receives it. The console
// interpolates the detail keys, so they are part of the wire contract. The
// duration is the one measured value and is zeroed before comparing.
func requireWire(t *testing.T, checks []testconn.Check, name, want string) {
	t.Helper()
	for _, c := range checks {
		if c.Name != name {
			continue
		}
		c.DurationMS = 0
		got, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s wire frame\n got %s\nwant %s", name, got, want)
		}
		return
	}
	t.Fatalf("check %s not reported", name)
}

func TestConnectionChecksWireDetail(t *testing.T) {
	t.Run("modern", func(t *testing.T) {
		checks, _ := runConnectionTest(t, testconnCloud(t, testconnHealthyReplies()))
		requireWire(t, checks, testconn.CheckAuthRequest, `{"seq":8,"name":"auth_request","status":"ok","duration_ms":0,"detail":{"auth":"access_key","hosts":2}}`)
		requireWire(t, checks, testconn.CheckProtocolShape, `{"seq":9,"name":"protocol_shape","status":"ok","duration_ms":0,"detail":{"allocation_field":"present","cpu_reservation_config":"bound","shape":"modern"}}`)
		requireWire(t, checks, testconn.CheckAPIEndpoints, `{"seq":10,"name":"api_endpoints","status":"ok","duration_ms":0}`)
		requireWire(t, checks, testconn.CheckReadPermissions, `{"seq":11,"name":"read_permissions","status":"ok","duration_ms":0,"detail":{"queries":"all"}}`)
		requireWire(t, checks, testconn.CheckResourceSummary, `{"seq":12,"name":"resource_summary","status":"ok","duration_ms":0,"detail":{"gpus":2,"gpus_free":1,"hosts":2}}`)
	})
	t.Run("legacy", func(t *testing.T) {
		replies := testconnHealthyReplies()
		delete(replies, "/v1/gpu-device/gpu-devices")
		replies["/v1/pci-device/pci-devices"] = fixtureReply{body: testconnPCIDevices}
		checks, _ := runConnectionTest(t, testconnCloud(t, replies))
		requireWire(t, checks, testconn.CheckProtocolShape, `{"seq":9,"name":"protocol_shape","status":"ok","duration_ms":0,"detail":{"cpu_reservation_config":"bound","shape":"legacy"}}`)
		requireWire(t, checks, testconn.CheckAPIEndpoints, `{"seq":10,"name":"api_endpoints","status":"ok","duration_ms":0,"detail":{"absent":["/gpu-device/gpu-devices"],"gpu_query":"pci_fallback"}}`)
	})
	t.Run("intermediate", func(t *testing.T) {
		replies := testconnHealthyReplies()
		replies["/v1/gpu-device/gpu-devices"] = fixtureReply{body: testconnPCIDevices}
		checks, _ := runConnectionTest(t, testconnCloud(t, replies))
		requireWire(t, checks, testconn.CheckProtocolShape, `{"seq":9,"name":"protocol_shape","status":"ok","duration_ms":0,"detail":{"allocation_field":"absent","cpu_reservation_config":"bound","shape":"intermediate"}}`)
	})
	t.Run("no gpus", func(t *testing.T) {
		replies := testconnHealthyReplies()
		replies["/v1/gpu-device/gpu-devices"] = fixtureReply{body: `{"inventories":[]}`}
		checks, _ := runConnectionTest(t, testconnCloud(t, replies))
		requireWire(t, checks, testconn.CheckProtocolShape, `{"seq":9,"name":"protocol_shape","status":"ok","duration_ms":0,"detail":{"allocation_field":"unobserved","cpu_reservation_config":"bound","shape":"gpu_query"}}`)
		requireWire(t, checks, testconn.CheckResourceSummary, `{"seq":12,"name":"resource_summary","status":"ok","duration_ms":0,"detail":{"gpus":0,"gpus_free":0,"hosts":2}}`)
	})
	t.Run("gpu routes missing", func(t *testing.T) {
		replies := testconnHealthyReplies()
		delete(replies, "/v1/gpu-device/gpu-devices")
		checks, _ := runConnectionTest(t, testconnCloud(t, replies))
		requireWire(t, checks, testconn.CheckProtocolShape, `{"seq":9,"name":"protocol_shape","status":"failed","duration_ms":0,"code":"ZSTACK_REQUEST_FAILED","detail":{"missing":["/gpu-device/gpu-devices","/pci-device/pci-devices"],"shape":"unsupported"},"hint":"zstack_version"}`)
		requireWire(t, checks, testconn.CheckAPIEndpoints, `{"seq":10,"name":"api_endpoints","status":"failed","duration_ms":0,"code":"ZSTACK_REQUEST_FAILED","detail":{"missing":["/gpu-device/gpu-devices","/pci-device/pci-devices"]},"hint":"zstack_version"}`)
	})
	t.Run("read denied", func(t *testing.T) {
		replies := testconnHealthyReplies()
		replies["/v1/gpu-device/gpu-devices"] = fixtureReply{status: http.StatusForbidden,
			body: `{"error":{"code":"SYS.1006","details":"api permission denied"}}`}
		checks, _ := runConnectionTest(t, testconnCloud(t, replies))
		requireWire(t, checks, testconn.CheckAPIEndpoints, `{"seq":10,"name":"api_endpoints","status":"ok","duration_ms":0,"detail":{"denied":["/gpu-device/gpu-devices"]}}`)
		requireWire(t, checks, testconn.CheckReadPermissions, `{"seq":11,"name":"read_permissions","status":"failed","duration_ms":0,"code":"ZSTACK_AUTH_REJECTED","detail":{"denied":["gpus"]},"hint":"zstack_permission"}`)
		requireWire(t, checks, testconn.CheckResourceSummary, `{"seq":12,"name":"resource_summary","status":"skipped","duration_ms":0,"detail":{"depends":"read_permissions"},"hint":"depends_failed"}`)
	})
	t.Run("auth rejected", func(t *testing.T) {
		replies := map[string]fixtureReply{
			"/v1/hosts": {status: http.StatusForbidden, body: `{"error":{"code":"SYS.1006","details":"signature check failed by the gateway"}}`},
		}
		checks, _ := runConnectionTest(t, testconnCloud(t, replies))
		requireWire(t, checks, testconn.CheckAuthRequest, `{"seq":8,"name":"auth_request","status":"failed","duration_ms":0,"code":"ZSTACK_AUTH_REJECTED"}`)
		requireWire(t, checks, testconn.CheckProtocolShape, `{"seq":9,"name":"protocol_shape","status":"skipped","duration_ms":0,"detail":{"depends":"auth_request"},"hint":"depends_failed"}`)
	})
}
