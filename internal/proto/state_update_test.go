package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestStateUpdatePreservesEmptyImageRefs(t *testing.T) {
	raw, err := json.Marshal(StateUpdate{ImageRefs: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"image_refs":[]`) {
		t.Fatalf("empty image refs were omitted: %s", raw)
	}
	var decoded StateUpdate
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ImageRefs == nil || len(decoded.ImageRefs) != 0 {
		t.Fatalf("decoded image refs = %#v", decoded.ImageRefs)
	}
}

func TestInstanceDesiredStatePreStartWireRoundTrip(t *testing.T) {
	want := InstanceDesiredState{
		UUID: "instance-1", ResolvedLaunch: ResolvedLaunch{PreStartArgv: []string{"prepare", "--mode", "safe"}},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"pre_start_argv":["prepare","--mode","safe"]`) || strings.Contains(string(raw), `"pre_start_script"`) {
		t.Fatalf("wire JSON = %s", raw)
	}
	var got InstanceDesiredState
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.PreStartArgv, want.PreStartArgv) || got.PreStartScript != "" {
		t.Fatalf("round trip = %#v", got)
	}

	raw = []byte(`{"uuid":"instance-2","launch":{"pre_start_script":"echo ready\n"}}`)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.PreStartScript != "echo ready\n" {
		t.Fatalf("script = %q", got.PreStartScript)
	}
}

func TestInstanceDesiredStateRuntimeAssetWireRoundTrip(t *testing.T) {
	want := InstanceDesiredState{UUID: "asset", ResolvedLaunch: ResolvedLaunch{RuntimeAssets: []RuntimeAsset{{
		File: "assets/runtime.tar.gz", SHA256: strings.Repeat("a", 64),
		SizeBytes: 4, UnpackTo: "/opt/gpuworks/app", PackageDigest: strings.Repeat("b", 64),
		ModelID: "gpuworks/runtime-wrapper", Content: []byte("gzip"),
	}}}}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`"runtime_assets":[{`, `"file":"assets/runtime.tar.gz"`,
		`"size_bytes":4`,
		`"package_digest":"` + strings.Repeat("b", 64) + `"`,
		`"model_id":"gpuworks/runtime-wrapper"`, `"content":"Z3ppcA=="`,
	} {
		if !strings.Contains(string(raw), fragment) {
			t.Fatalf("wire JSON %s lacks %s", raw, fragment)
		}
	}
	var got InstanceDesiredState
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RuntimeAssets, want.RuntimeAssets) {
		t.Fatalf("round trip=%+v", got.RuntimeAssets)
	}
	want.RuntimeAssets[0].Content = nil
	raw, err = json.Marshal(want)
	if err != nil || strings.Contains(string(raw), `"content"`) {
		t.Fatalf("contentless cache-hit contract=%s err=%v", raw, err)
	}
}

func TestRuntimeAssetCacheAcknowledgementWireContract(t *testing.T) {
	digest := strings.Repeat("a", 64)
	helloRaw, err := json.Marshal(Hello{RuntimeAssetDigests: []string{digest}})
	if err != nil || !strings.Contains(string(helloRaw), `"runtime_asset_digests":["`+digest+`"]`) {
		t.Fatalf("hello wire=%s err=%v", helloRaw, err)
	}
	statusRaw, err := json.Marshal(InstanceStatus{UUID: "instance", State: InstStarting, RuntimeAssetDigests: []string{digest}})
	if err != nil || !strings.Contains(string(statusRaw), `"runtime_asset_digests":["`+digest+`"]`) {
		t.Fatalf("status wire=%s err=%v", statusRaw, err)
	}
	var status InstanceStatus
	if err := json.Unmarshal(statusRaw, &status); err != nil || !reflect.DeepEqual(status.RuntimeAssetDigests, []string{digest}) {
		t.Fatalf("status round trip=%+v err=%v", status, err)
	}
}

func TestSleepFailureTargetWireContract(t *testing.T) {
	target := SleepLevelAwake
	want := InstanceStatus{UUID: "instance", State: InstRunning, SleepLevel: SleepLevelL1, SleepTarget: &target, Phase: PhaseSleep, Message: "wake failed"}
	raw, err := json.Marshal(want)
	if err != nil || !strings.Contains(string(raw), `"sleep_target":0`) {
		t.Fatalf("sleep failure wire=%s err=%v", raw, err)
	}
	var got InstanceStatus
	if err := json.Unmarshal(raw, &got); err != nil || got.SleepTarget == nil || *got.SleepTarget != target {
		t.Fatalf("sleep failure round trip=%+v err=%v", got, err)
	}
}

// l1_host_buffer_held travels only when true; an older agent's report, which
// lacks the key, reads as not known to hold one.
func TestL1HostBufferHeldWireContract(t *testing.T) {
	raw, err := json.Marshal(InstanceStatus{UUID: "instance", State: InstRunning, L1HostBufferHeld: true})
	if err != nil || !strings.Contains(string(raw), `"l1_host_buffer_held":true`) {
		t.Fatalf("held wire=%s err=%v", raw, err)
	}
	var got InstanceStatus
	if err := json.Unmarshal(raw, &got); err != nil || !got.L1HostBufferHeld {
		t.Fatalf("held round trip=%+v err=%v", got, err)
	}
	raw, err = json.Marshal(InstanceStatus{UUID: "instance", State: InstRunning})
	if err != nil || strings.Contains(string(raw), "l1_host_buffer_held") {
		t.Fatalf("not held wire=%s err=%v", raw, err)
	}
	var old InstanceStatus
	if err := json.Unmarshal([]byte(`{"uuid":"instance","state":"running","sleep_level":1}`), &old); err != nil || old.L1HostBufferHeld {
		t.Fatalf("older agent report=%+v err=%v", old, err)
	}
}

func TestModelFileTaskSubfolderWireContract(t *testing.T) {
	want := ModelFileTask{
		Key: "9-2", ModelID: 9, Source: "modelscope", SourceID: "MiniMax/MiniMax-H3",
		Revision: "commit", Subfolder: "FL2VA", TargetPath: "modelscope/h3-sparse", DesiredState: "present",
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	const expected = `{"key":"9-2","model_id":9,"source":"modelscope","source_id":"MiniMax/MiniMax-H3","revision":"commit","subfolder":"FL2VA","target_path":"modelscope/h3-sparse","desired_state":"present","rate_limit_kbs":0}`
	if string(raw) != expected {
		t.Fatalf("model file task wire=%s, want %s", raw, expected)
	}
	var got ModelFileTask
	if err := json.Unmarshal(raw, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("model file task round trip=%+v err=%v", got, err)
	}
}

// The /dev/shm size is part of the resolved launch the preview shows, so it
// travels inside "launch" and nowhere else on the wire.
func TestInstanceDesiredStateShmTravelsInsideTheLaunch(t *testing.T) {
	raw, err := json.Marshal(InstanceDesiredState{UUID: "shm", ResolvedLaunch: ResolvedLaunch{ShmBytes: 32 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ShmBytes *int64 `json:"shm_bytes"`
		Launch   struct {
			ShmBytes int64 `json:"shm_bytes"`
		} `json:"launch"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ShmBytes != nil || wire.Launch.ShmBytes != 32<<30 {
		t.Fatalf("wire JSON = %s", raw)
	}
	var got InstanceDesiredState
	if err := json.Unmarshal([]byte(`{"uuid":"shm","launch":{"argv":[],"model_mount":"/model","shm_bytes":1073741824}}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.ShmBytes != 1<<30 {
		t.Fatalf("decoded shm = %d", got.ShmBytes)
	}
}
