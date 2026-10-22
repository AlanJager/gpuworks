package compshare

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// TestSign checks the UCloud SHA1 signature against a precomputed vector.
// Vector: sha1("ActionDescribeCompShareInstanceLimit1PublicKeypubkey123" +
// "privkey456") computed independently with sha1sum.
func TestSign(t *testing.T) {
	params := map[string]string{
		"Action":    "DescribeCompShareInstance",
		"Limit":     "1",
		"PublicKey": "pubkey123",
	}
	const want = "1a2b56262d1b13f8f8815e6b878e03ab78a14a67"
	if got := sign(params, "privkey456"); got != want {
		t.Fatalf("sign = %q, want %q", got, want)
	}
	// Signature must be independent of map iteration order (stable).
	if got := sign(params, "privkey456"); got != want {
		t.Fatalf("sign not stable: %q", got)
	}
}

func TestCallCanReuseParamsWithoutCorruptingSignature(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		got := make(map[string]string, len(r.Form))
		for key := range r.Form {
			if key != "Signature" {
				got[key] = r.Form.Get(key)
			}
		}
		if want := sign(got, "priv"); r.Form.Get("Signature") != want {
			t.Errorf("request %d signature=%q, want %q", requests, r.Form.Get("Signature"), want)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0})
	}))
	defer srv.Close()

	d := New()
	d.setBaseURL(srv.URL)
	params := map[string]string{"Action": "CreateCompShareInstance", "Region": "cn-wlcb"}
	for range 2 {
		if _, err := d.call(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, params); err != nil {
			t.Fatal(err)
		}
	}
	if len(params) != 2 || params["PublicKey"] != "" || params["Signature"] != "" {
		t.Fatalf("call mutated reusable params: %#v", params)
	}
}

func TestCompshareAcceleratedDNSRegions(t *testing.T) {
	for _, region := range []string{"华北一C", "上海二A", "上海二B", "华北二A", "华北二C", "cn-wlcb"} {
		if !compshareAcceleratedRegion(region) || !strings.Contains(acceleratedDNSCommand(region), "100.90.90.90") {
			t.Errorf("region %q did not select UAAA DNS", region)
		}
	}
	for _, region := range []string{"华东一A", "cn-unknown", ""} {
		if compshareAcceleratedRegion(region) || acceleratedDNSCommand(region) != "" {
			t.Errorf("region %q unexpectedly selected UAAA DNS", region)
		}
	}
}

func TestKindForCompshareProducts(t *testing.T) {
	for _, tc := range []struct {
		machineType string
		want        string
	}{
		{machineType: "", want: "vm"},
		{machineType: "G", want: "vm"},
		{machineType: "VM", want: "vm"},
		{machineType: "Container", want: "container"},
		{machineType: "gpu-container", want: "container"},
	} {
		if got := kindFor(tc.machineType); got != tc.want {
			t.Errorf("kindFor(%q)=%q, want %q", tc.machineType, got, tc.want)
		}
	}
}

func TestMapInstanceUsesPodTypeAndDynamicSSH(t *testing.T) {
	got := mapInstance(ucloudInstance{
		UHostId: "cpod-1", Region: "cn-sh2", Zone: "cn-sh2-01", State: "Running",
		GPU: 1, GpuType: "5090", MachineType: "G", InstanceType: "Container",
		SshLoginCommand: "ssh -p 26042 root@podtcp.compshare.cn",
	})
	if got.Kind != "container" || got.Region != "cn-sh2" || got.Zone != "cn-sh2-01" {
		t.Fatalf("pod mapping = %+v", got)
	}
	if got.SSHPort != 26042 || got.PublicIP != "podtcp.compshare.cn" {
		t.Fatalf("pod ssh mapping = %+v", got)
	}
}

func TestPowerOffUsesDiscoveredInstanceZone(t *testing.T) {
	var stopRegion, stopZone string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("Action") {
		case listAction:
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostSet": []map[string]any{{"UHostId": "cpod-1", "Region": "cn-sh2", "Zone": "cn-sh2-01", "State": "Running", "InstanceType": "Container"}}})
		case "StopCompShareInstance":
			stopRegion, stopZone = r.Form.Get("Region"), r.Form.Get("Zone")
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0})
		default:
			t.Errorf("unexpected action %q", r.Form.Get("Action"))
		}
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	if err := d.PowerOff(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "cpod-1"); err != nil {
		t.Fatal(err)
	}
	if stopRegion != "cn-sh2" || stopZone != "cn-sh2-01" {
		t.Fatalf("stop placement = %s/%s", stopRegion, stopZone)
	}
}

func TestListInstances(t *testing.T) {
	sample := map[string]any{
		"Action":     "DescribeCompShareInstanceResponse",
		"RetCode":    0,
		"TotalCount": 1,
		"UHostSet": []map[string]any{
			{
				"UHostId":         "uhost-abc",
				"Name":            "gpu-1",
				"State":           "Running",
				"Region":          "cn-wlcb",
				"Zone":            "cn-wlcb-01",
				"GPU":             2,
				"GpuType":         "4090",
				"InstancePrice":   12.5,
				"SshLoginCommand": "ssh root@10.0.0.5 -p 22",
				"IPSet": []map[string]any{
					{"Type": "International", "IP": "203.0.113.9"},
					{"Type": "Private", "IP": "10.0.0.5"},
				},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		q := r.Form
		if q.Get("Signature") == "" || q.Get("PublicKey") == "" {
			t.Errorf("missing Signature/PublicKey in query: %v", q)
		}
		if q.Get("Action") != listAction {
			t.Errorf("Action = %q", q.Get("Action"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sample)
	}))
	defer srv.Close()

	d := New()
	d.setBaseURL(srv.URL)
	creds := provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}

	if err := d.ValidateCredentials(context.Background(), creds); err != nil {
		t.Fatalf("ValidateCredentials: %v", err)
	}

	got, err := d.ListInstances(context.Background(), creds)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d instances, want 1", len(got))
	}
	in := got[0]
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"ExternalID", in.ExternalID, "uhost-abc"},
		{"Name", in.Name, "gpu-1"},
		{"GPUModel", in.GPUModel, "4090"},
		{"GPUCount", in.GPUCount, 2},
		{"Power", in.Power, provider.PowerRunning},
		{"PricePerHr", in.PricePerHr, 12.5},
		{"Region", in.Region, "cn-wlcb"},
		{"PublicIP", in.PublicIP, "203.0.113.9"},
		{"PrivateIP", in.PrivateIP, "10.0.0.5"},
		{"SSHPort", in.SSHPort, 22},
		{"SSHUser", in.SSHUser, "root"},
		{"Kind", in.Kind, "vm"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestCreateInstanceContract(t *testing.T) {
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("request = %s content-type=%q", r.Method, r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, r.Form.Get("Action"))
		if r.Form.Get("Action") == "CheckCompShareResourceCapacity" {
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "Specs": []map[string]any{{"Gpu": 1, "Cpu": 16, "Mem": 64, "ResourceEnough": true}}})
			return
		}
		want := map[string]string{"Action": "CreateCompShareInstance", "Region": "cn-wlcb", "Zone": "cn-wlcb-01", "MachineType": "G", "CompShareImageId": "compshareImage-test", "GPU": "1", "GpuType": "4090", "MinimalCpuPlatform": "Auto", "CPU": "16", "Memory": "65536", "Disks.0.IsBoot": "true", "Disks.0.Size": "120", "Disks.0.Type": "CLOUD_SSD", "ChargeType": "Postpay", "Name": "gpuworks-test-run"}
		for key, value := range want {
			if r.Form.Get(key) != value {
				t.Errorf("%s=%q want=%q", key, r.Form.Get(key), value)
			}
		}
		if got, _ := base64.StdEncoding.DecodeString(r.Form.Get("Password")); string(got) != "Password123!" {
			t.Errorf("password=%q", got)
		}
		if r.Form.Get("PublicKey") == "" || r.Form.Get("Signature") == "" {
			t.Error("missing authentication fields")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostIds": []string{"uhost-new"}})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	inst, err := d.CreateInstance(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, provider.CreateInstanceRequest{Name: "gpuworks-test-run", GPUModel: "RTX4090", GPUCount: 1, Image: "compshareImage-test", Extra: map[string]string{"disk_gb": "120", "password": "Password123!"}})
	if err != nil {
		t.Fatal(err)
	}
	if inst.ExternalID != "uhost-new" || inst.GPUModel != "4090" || inst.GPUCount != 1 {
		t.Fatalf("instance=%+v", inst)
	}
	if len(actions) != 2 || actions[0] != "CheckCompShareResourceCapacity" || actions[1] != "CreateCompShareInstance" {
		t.Fatalf("actions=%v", actions)
	}
}

func TestCheckResourceCapacityContract(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		got = map[string]string{}
		for key := range r.Form {
			got[key] = r.Form.Get(key)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "Specs": []map[string]any{
			{"Gpu": 1, "Cpu": 12, "Mem": 32, "ResourceEnough": true},
			{"Gpu": 2, "Cpu": 24, "Mem": 64, "ResourceEnough": false},
		}})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	rows, err := d.CheckResourceCapacity(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, ResourceCapacityRequest{GPUModel: "NVIDIA GeForce RTX 3080 Ti", Image: "image-test", DiskGB: 120})
	if err != nil {
		t.Fatal(err)
	}
	if got["Action"] != "CheckCompShareResourceCapacity" || got["GpuType"] != "3080Ti" || got["Disks.0.Size"] != "120" {
		t.Fatalf("request=%v", got)
	}
	if len(rows) != 2 || rows[0].GPU != 1 || rows[0].MemoryGB != 32 || !rows[0].ResourceEnough || rows[1].ResourceEnough {
		t.Fatalf("capacity=%+v", rows)
	}
}

// TestCheckResourceCapacityRetriesServiceError pins RetCode 8433 ("Service
// error, please retry") as transient: CI saw a whole zone's capacity answers
// vanish for a query round on 2026-09-24, so the query must retry instead of
// reporting the zone as empty.
func TestCheckResourceCapacityRetriesServiceError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		calls++
		if calls == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 8433, "Message": "Service error, please retry or contact technical support"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "Specs": []map[string]any{{"Gpu": 1, "Cpu": 16, "Mem": 64, "ResourceEnough": true}}})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	rows, err := d.CheckResourceCapacity(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, ResourceCapacityRequest{GPUModel: "4090", Image: "image-test", DiskGB: 160})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(rows) != 1 || !rows[0].ResourceEnough {
		t.Fatalf("calls=%d rows=%+v", calls, rows)
	}
}

func TestCustomImageContracts(t *testing.T) {
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		action := r.Form.Get("Action")
		actions = append(actions, action)
		if r.Form.Get("PublicKey") == "" || r.Form.Get("Signature") == "" {
			t.Error("missing authentication fields")
		}
		switch action {
		case "CreateCompShareCustomImage":
			want := map[string]string{
				"Region": "cn-wlcb", "Zone": "cn-wlcb-01", "UHostId": "uhost-source",
				"Name": "gpuworks-golden", "Description": "preloaded fixture",
			}
			for key, value := range want {
				if got := r.Form.Get(key); got != value {
					t.Errorf("%s=%q want=%q", key, got, value)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "CompShareImageId": "image-new"})
		case "GetCompShareImageCreateProgress":
			if got := r.Form.Get("CompShareImageId"); got != "image-new" {
				t.Errorf("CompShareImageId=%q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "Process": 75.5})
		case "DescribeCompShareCustomImages":
			if got := r.Form.Get("CompShareImageId"); got != "image-new" {
				t.Errorf("CompShareImageId=%q", got)
			}
			if r.Form.Get("Offset") != "0" || r.Form.Get("Limit") != "100" {
				t.Errorf("pagination=%q/%q", r.Form.Get("Offset"), r.Form.Get("Limit"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "ImageSet": []map[string]any{{
				"CompShareImageId": "image-new", "Name": "gpuworks-golden", "Status": "Available", "CreateTime": 123,
			}}})
		default:
			t.Fatalf("unexpected action %q", action)
		}
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	creds := provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}
	imageID, err := d.CreateCustomImage(t.Context(), creds, CustomImageRequest{
		InstanceID: "uhost-source", Name: "gpuworks-golden", Description: "preloaded fixture",
	})
	if err != nil || imageID != "image-new" {
		t.Fatalf("CreateCustomImage id=%q err=%v", imageID, err)
	}
	progress, err := d.CustomImageProgress(t.Context(), creds, imageID, "", "")
	if err != nil || progress != 75.5 {
		t.Fatalf("CustomImageProgress=%v err=%v", progress, err)
	}
	images, err := d.ListCustomImages(t.Context(), creds, imageID, "")
	if err != nil || len(images) != 1 || images[0].ID != imageID || images[0].Status != "Available" || images[0].CreateTime != 123 {
		t.Fatalf("ListCustomImages=%+v err=%v", images, err)
	}
	if strings.Join(actions, ",") != "CreateCompShareCustomImage,GetCompShareImageCreateProgress,DescribeCompShareCustomImages" {
		t.Fatalf("actions=%v", actions)
	}
}

func TestCustomImageValidation(t *testing.T) {
	d := New()
	creds := provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}
	if _, err := d.CreateCustomImage(t.Context(), creds, CustomImageRequest{}); err == nil {
		t.Fatal("CreateCustomImage accepted an empty source")
	}
	if _, err := d.CreateCustomImage(t.Context(), creds, CustomImageRequest{InstanceID: "uhost"}); err == nil {
		t.Fatal("CreateCustomImage accepted an empty name")
	}
	if _, err := d.CustomImageProgress(t.Context(), creds, "", "", ""); err == nil {
		t.Fatal("CustomImageProgress accepted an empty image id")
	}
}

func TestCreateInstanceRejectsUnavailableCapacity(t *testing.T) {
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		actions = append(actions, r.Form.Get("Action"))
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "Specs": []map[string]any{{"Gpu": 1, "Cpu": 16, "Mem": 64, "ResourceEnough": false}}})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	_, err := d.CreateInstance(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, provider.CreateInstanceRequest{GPUModel: "RTX4090", GPUCount: 1, Image: "compshareImage-test"})
	if err == nil || len(actions) != 1 || actions[0] != "CheckCompShareResourceCapacity" {
		t.Fatalf("err=%v actions=%v", err, actions)
	}
}

func TestCreateInstanceSelectsAvailableCapacity(t *testing.T) {
	var createdCPU, createdMemory string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch r.Form.Get("Action") {
		case "CheckCompShareResourceCapacity":
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "Specs": []map[string]any{
				{"Gpu": 1, "Cpu": 32, "Mem": 128, "ResourceEnough": true},
				{"Gpu": 1, "Cpu": 8, "Mem": 32, "ResourceEnough": true},
				{"Gpu": 1, "Cpu": 4, "Mem": 16, "ResourceEnough": false},
			}})
		case "CreateCompShareInstance":
			createdCPU, createdMemory = r.Form.Get("CPU"), r.Form.Get("Memory")
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostIds": []string{"uhost-selected"}})
		default:
			t.Errorf("unexpected action %q", r.Form.Get("Action"))
		}
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	if _, err := d.CreateInstance(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, provider.CreateInstanceRequest{GPUModel: "RTX4090", GPUCount: 1, Image: "compshareImage-test"}); err != nil {
		t.Fatal(err)
	}
	if createdCPU != "8" || createdMemory != "32768" {
		t.Fatalf("selected cpu=%s memory=%s", createdCPU, createdMemory)
	}
}

func TestCreateInstanceHonorsExplicitCapacity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "Specs": []map[string]any{{"Gpu": 1, "Cpu": 8, "Mem": 32, "ResourceEnough": true}}})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	_, err := d.CreateInstance(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, provider.CreateInstanceRequest{GPUModel: "RTX4090", GPUCount: 1, Image: "compshareImage-test", Extra: map[string]string{"cpu": "16", "memory_mb": "65536"}})
	if err == nil {
		t.Fatal("CreateInstance accepted a different capacity than explicitly requested")
	}
}

func TestDeleteInstanceStopsBeforeTerminate(t *testing.T) {
	state := "Running"
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		action := r.Form.Get("Action")
		actions = append(actions, action)
		switch action {
		case listAction:
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostSet": []map[string]any{{"UHostId": "uhost-1", "State": state}}})
		case "StopCompShareInstance":
			state = "Stopped"
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0})
		case "TerminateCompShareInstance":
			if r.Form.Get("ReleaseUDisk") != "true" {
				t.Error("ReleaseUDisk is not true")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostId": "uhost-1"})
		default:
			t.Errorf("unexpected action %q", action)
		}
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	if err := d.DeleteInstance(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "uhost-1"); err != nil {
		t.Fatal(err)
	}
	want := []string{listAction, "StopCompShareInstance", listAction, "TerminateCompShareInstance"}
	if len(actions) != len(want) {
		t.Fatalf("actions=%v", actions)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("actions=%v want=%v", actions, want)
		}
	}
}

func TestDeleteInstanceIsIdempotentWhenInstanceIsAbsent(t *testing.T) {
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		actions = append(actions, r.Form.Get("Action"))
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostSet": []any{}})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	if err := d.DeleteInstance(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "uhost-gone"); err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0] != listAction {
		t.Fatalf("actions=%v", actions)
	}
}

func TestDeleteInstanceToleratesTerminateNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("Action") == listAction {
			_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostSet": []map[string]any{{"UHostId": "uhost-race", "State": "Stopped"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 190, "Message": "instance does not exist"})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	if err := d.DeleteInstance(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "uhost-race"); err != nil {
		t.Fatal(err)
	}
}

func TestSSHAccessDecodesPasswordAndPrefersLoginCommand(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("Password123!"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostSet": []map[string]any{{
			"UHostId": "uhost-ssh", "Password": encoded,
			"SshLoginCommand": "ssh ubuntu@203.0.113.8 -p 60022",
			"IPSet":           []map[string]any{{"Type": "International", "IP": "198.51.100.4"}},
		}}})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	host, port, user, password, err := d.SSHAccess(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "uhost-ssh")
	if err != nil {
		t.Fatal(err)
	}
	if host != "203.0.113.8" || port != 60022 || user != "ubuntu" || password != "Password123!" {
		t.Fatalf("access=%s:%d user=%q password_ok=%t", host, port, user, password == "Password123!")
	}
}

func TestSSHAccessUsesPublicIPWhenLoginCommandIsPrivate(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("Password123!"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostSet": []map[string]any{{
			"UHostId": "uhost-private-ssh", "Password": encoded,
			"SshLoginCommand": "ssh root@10.60.185.114 -p 22",
			"IPSet": []map[string]any{
				{"Type": "Private", "IP": "10.60.185.114"},
				{"Type": "International", "IP": "198.51.100.21"},
			},
		}}})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	host, port, user, password, err := d.SSHAccess(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "uhost-private-ssh")
	if err != nil {
		t.Fatal(err)
	}
	if host != "198.51.100.21" || port != 22 || user != "root" || password != "Password123!" {
		t.Fatalf("access=%s:%d user=%q password_ok=%t", host, port, user, password == "Password123!")
	}
}

func TestPreferredManagementSSHHostPreservesRequiredFallbacks(t *testing.T) {
	addresses := []compshareIP{{Type: "Private", IP: "10.0.0.8"}, {Type: "International", IP: "203.0.113.8"}}
	for _, tc := range []struct {
		name, loginHost, want string
	}{
		{name: "public command", loginHost: "198.51.100.8", want: "198.51.100.8"},
		{name: "proxy command", loginHost: "ssh.example.test", want: "ssh.example.test"},
		{name: "private command", loginHost: "10.0.0.8", want: "203.0.113.8"},
		{name: "loopback command", loginHost: "127.0.0.1", want: "203.0.113.8"},
		{name: "missing command", want: "203.0.113.8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := preferredManagementSSHHost(tc.loginHost, addresses); got != tc.want {
				t.Fatalf("preferredManagementSSHHost(%q)=%q, want %q", tc.loginHost, got, tc.want)
			}
		})
	}
	if got := preferredManagementSSHHost("10.0.0.8", addresses[:1]); got != "10.0.0.8" {
		t.Fatalf("private-only fallback=%q, want original private endpoint", got)
	}
}

func TestParseSSHLoginCommand(t *testing.T) {
	host, port, user := parseSSHLoginCommand("ssh root@203.0.113.9 -p 60022")
	if host != "203.0.113.9" || port != 60022 || user != "root" {
		t.Fatalf("got %s:%d user=%s", host, port, user)
	}
}

func TestNormalizeGPUType(t *testing.T) {
	cases := map[string]string{
		"4090": "4090", "RTX 4090": "4090", "NVIDIA GeForce RTX 4090": "4090",
		"3090": "3090", "RTX 3090": "3090", "NVIDIA GeForce RTX 3090": "3090",
		"3080Ti": "3080Ti", "RTX 3080 Ti": "3080Ti", "NVIDIA GeForce RTX 3080 Ti": "3080Ti",
	}
	for input, want := range cases {
		if got := normalizeGPUType(input); got != want {
			t.Errorf("normalizeGPUType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPowerFromState(t *testing.T) {
	cases := map[string]provider.PowerState{
		"Running": provider.PowerRunning,
		"Stopped": provider.PowerStopped,
		"Install": provider.PowerUnknown,
	}
	for in, want := range cases {
		if got := powerFromState(in); got != want {
			t.Errorf("powerFromState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPublicPrivateFallback(t *testing.T) {
	pub, priv := publicPrivate(provider.Credentials{APIKey: "abc.def"})
	if pub != "abc" || priv != "def" {
		t.Fatalf("combined APIKey fallback = %q/%q", pub, priv)
	}
}

func TestInstallAgentErrors(t *testing.T) {
	d := New()
	err := d.InstallAgent(context.Background(), provider.Credentials{}, provider.InstallRequest{})
	if err == nil {
		t.Fatal("expected error when SSHKeyPEM missing")
	}
}

func TestRegistered(t *testing.T) {
	if _, ok := provider.Get("compshare"); !ok {
		t.Fatal("compshare driver not registered")
	}
}

func TestCapabilities(t *testing.T) {
	caps := New().Capabilities()
	if !caps.CanListInstances || !caps.CanPowerControl || !caps.CanInstallAgent || !caps.CanCreate || !caps.CanDelete || !caps.HasPricing {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
}

// CompShare offers only password login; the generated password is what the
// server logs in with to authorize the platform key.
func TestInstancePasswordIsTheGeneratedRootPassword(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("Gen3rated!"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.Form.Get("Action"); got != listAction {
			t.Errorf("request Action=%q", got)
		}
		// Like the real API, an unknown id lists nothing rather than failing.
		set := []map[string]any{}
		if r.Form.Get("UHostIds.0") == "uhost-1" {
			set = append(set, map[string]any{"UHostId": "uhost-1", "Password": encoded, "SshLoginCommand": "ssh root@203.0.113.9"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"RetCode": 0, "UHostSet": set})
	}))
	defer srv.Close()
	d := New()
	d.setBaseURL(srv.URL)
	password, err := d.InstancePassword(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "uhost-1")
	if err != nil || password != "Gen3rated!" {
		t.Fatalf("password ok=%t err=%v", password == "Gen3rated!", err)
	}
	if _, err := d.InstancePassword(t.Context(), provider.Credentials{PublicKey: "pub", PrivateKey: "priv"}, "uhost-missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing instance err = %v", err)
	}
}
