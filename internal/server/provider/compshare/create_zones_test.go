package compshare

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// zoneFixture answers like api.compshare.cn did on 2026-10-26: three
// container (IsPod) zones, each configured for a different GpuType. A GpuType
// a zone is not configured for is refused with RetCode 230 (not an empty
// capacity answer); a configured one lists specs with ResourceEnough.
type zoneFixture struct {
	mu      sync.Mutex
	checks  []string // "zone/GpuType" per CheckCompShareResourceCapacity
	creates []string // "zone/GpuType" per CreateCompShareInstance
}

var fixtureZones = []map[string]any{
	{"Region": "cn-wlcb", "Zone": "cn-wlcb-01", "IsPod": false},
	{"Region": "cn-sh2", "Zone": "cn-sh2-02", "IsPod": false},
	{"Region": "cn-bj2", "Zone": "cn-bj2-03", "IsPod": true},
	{"Region": "cn-sh2", "Zone": "cn-sh2-01", "IsPod": true},
	{"Region": "cn-sh2", "Zone": "cn-sh2-03", "IsPod": true},
}

// zoneGPU maps zone -> GpuType -> ResourceEnough.
var zoneGPU = map[string]map[string]bool{
	"cn-bj2-03":  {"4090": true},
	"cn-sh2-01":  {"5090": false},
	"cn-sh2-03":  {"4090_48G": true},
	"cn-wlcb-01": {"3090": true},
}

func (f *zoneFixture) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		zone, gpu := r.Form.Get("Zone"), r.Form.Get("GpuType")
		switch r.Form.Get("Action") {
		case "DescribeCompShareSupportZone":
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "ZoneInfo": fixtureZones})
		case "CheckCompShareResourceCapacity":
			f.checks = append(f.checks, zone+"/"+gpu)
			enough, ok := zoneGPU[zone][gpu]
			if !ok {
				_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 230, "Message": "Params [GpuType] not available machine type not configured for zone"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "Specs": []map[string]any{{"Gpu": 1, "Cpu": 14, "Mem": 32, "ResourceEnough": enough}}})
		case "CreateCompShareInstance":
			f.creates = append(f.creates, zone+"/"+gpu)
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostIds": []string{"uhost-" + zone}})
		default:
			t.Errorf("unexpected action %q", r.Form.Get("Action"))
		}
	})
}

func newZoneDriver(t *testing.T) (*Driver, *zoneFixture) {
	t.Helper()
	f := &zoneFixture{}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	d := New()
	d.setBaseURL(srv.URL)
	d.zoneDiscovery = true
	return d, f
}

func createIn(t *testing.T, d *Driver, kind, gpu string, extra map[string]string) (provider.Instance, error) {
	t.Helper()
	if extra == nil {
		extra = map[string]string{}
	}
	extra["instance_kind"] = kind
	return d.CreateInstance(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, provider.CreateInstanceRequest{GPUModel: gpu, GPUCount: 1, Image: "compshareImage-container", Extra: extra})
}

func TestCreateContainerWalksEveryPodZone(t *testing.T) {
	cases := []struct {
		gpu        string
		wantZone   string
		wantChecks []string
	}{
		// Each type is configured in exactly one pod zone; the create must
		// reach it whichever position it has in the zone list.
		{"4090", "cn-bj2-03", []string{"cn-bj2-03/4090"}},
		{"4090_48G", "cn-sh2-03", []string{"cn-bj2-03/4090_48G", "cn-sh2-01/4090_48G", "cn-sh2-03/4090_48G"}},
	}
	for _, tc := range cases {
		t.Run(tc.gpu, func(t *testing.T) {
			d, f := newZoneDriver(t)
			inst, err := createIn(t, d, "container", tc.gpu, nil)
			if err != nil {
				t.Fatal(err)
			}
			if inst.Zone != tc.wantZone || inst.Kind != "container" || inst.ExternalID != "uhost-"+tc.wantZone {
				t.Fatalf("instance=%+v, want container in %s", inst, tc.wantZone)
			}
			if strings.Join(f.checks, ",") != strings.Join(tc.wantChecks, ",") {
				t.Fatalf("capacity checks=%v, want %v (VM zones must be skipped)", f.checks, tc.wantChecks)
			}
			if len(f.creates) != 1 || f.creates[0] != tc.wantZone+"/"+tc.gpu {
				t.Fatalf("creates=%v", f.creates)
			}
		})
	}
}

func TestCreateContainerReportsEveryZoneWhenNoneFits(t *testing.T) {
	// 5090 exists only in cn-sh2-01 and has no capacity there; 3090 only in
	// a VM zone, which a container never looks at.
	for _, gpu := range []string{"5090", "3090"} {
		t.Run(gpu, func(t *testing.T) {
			d, f := newZoneDriver(t)
			_, err := createIn(t, d, "container", gpu, nil)
			if err == nil {
				t.Fatal("container created although no pod zone can host it")
			}
			msg := err.Error()
			for _, zone := range []string{"cn-bj2-03", "cn-sh2-01", "cn-sh2-03"} {
				if !strings.Contains(msg, zone) {
					t.Errorf("error %q does not mention pod zone %s", msg, zone)
				}
			}
			if strings.Contains(msg, "cn-wlcb-01") || strings.Contains(msg, "cn-sh2-02") {
				t.Errorf("error %q mentions a VM zone", msg)
			}
			// Callers classify the refusal by substring; each zone's cause stays visible.
			if !strings.Contains(msg, "RetCode 230") {
				t.Errorf("error %q lost the RetCode 230 cause", msg)
			}
			if gpu == "5090" && !strings.Contains(msg, "requested capacity unavailable in cn-sh2/cn-sh2-01") {
				t.Errorf("error %q does not name the zone that is out of capacity", msg)
			}
			if len(f.checks) != 3 || len(f.creates) != 0 {
				t.Errorf("checks=%v creates=%v, want all three pod zones checked and nothing created", f.checks, f.creates)
			}
		})
	}
}

func TestCreateVMKeepsToVMZones(t *testing.T) {
	d, f := newZoneDriver(t)
	inst, err := createIn(t, d, "vm", "3090", nil)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Zone != "cn-wlcb-01" || inst.Kind != "vm" {
		t.Fatalf("instance=%+v", inst)
	}
	if strings.Join(f.checks, ",") != "cn-wlcb-01/3090" {
		t.Fatalf("checks=%v", f.checks)
	}
}

func TestCreateContainerHonorsPinnedZone(t *testing.T) {
	d, f := newZoneDriver(t)
	_, err := createIn(t, d, "container", "4090", map[string]string{"region": "cn-sh2", "zone": "cn-sh2-03"})
	if err == nil || !strings.Contains(err.Error(), "RetCode 230") {
		t.Fatalf("err=%v, want RetCode 230 from the pinned zone", err)
	}
	if strings.Join(f.checks, ",") != "cn-sh2-03/4090" {
		t.Fatalf("a pinned zone must be the only one tried; checks=%v", f.checks)
	}
}
