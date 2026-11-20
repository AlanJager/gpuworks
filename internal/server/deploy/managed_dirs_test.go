package deploy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestNormalizeManagedDirsTrimsAndWritesTheDefaultRetention(t *testing.T) {
	dirs := []ManagedDir{{Name: " pico-state ", MountPath: " /pico ", LimitBytes: 400 << 30}, {Name: "logs", MountPath: "/var/log/pico", LimitBytes: 1 << 30, Retain: "Deployment"}}
	if err := NormalizeManagedDirs(dirs); err != nil {
		t.Fatal(err)
	}
	want := []ManagedDir{{Name: "pico-state", MountPath: "/pico", LimitBytes: 400 << 30, Retain: "deployment"}, {Name: "logs", MountPath: "/var/log/pico", LimitBytes: 1 << 30, Retain: "deployment"}}
	if !reflect.DeepEqual(dirs, want) {
		t.Fatalf("dirs = %+v", dirs)
	}
	if ManagedDirBytes(dirs) != 401<<30 {
		t.Fatalf("bytes = %d", ManagedDirBytes(dirs))
	}
}

func TestNormalizeManagedDirsRefusesWhatNoNodeCouldKeep(t *testing.T) {
	many := make([]ManagedDir, MaxManagedDirs+1)
	for index := range many {
		many[index] = ManagedDir{Name: "d" + string(rune('a'+index)), MountPath: "/d" + string(rune('a'+index)), LimitBytes: 1}
	}
	for name, test := range map[string]struct {
		dirs []ManagedDir
		want string
	}{
		"too many":         {many, "at most"},
		"empty name":       {[]ManagedDir{{MountPath: "/a", LimitBytes: 1}}, "name"},
		"upper-case name":  {[]ManagedDir{{Name: "State", MountPath: "/a", LimitBytes: 1}}, "name"},
		"path in name":     {[]ManagedDir{{Name: "../etc", MountPath: "/a", LimitBytes: 1}}, "name"},
		"duplicate name":   {[]ManagedDir{{Name: "a", MountPath: "/a", LimitBytes: 1}, {Name: "a", MountPath: "/b", LimitBytes: 1}}, "duplicate"},
		"relative path":    {[]ManagedDir{{Name: "a", MountPath: "a", LimitBytes: 1}}, "mount_path"},
		"unclean path":     {[]ManagedDir{{Name: "a", MountPath: "/a/../b", LimitBytes: 1}}, "mount_path"},
		"root path":        {[]ManagedDir{{Name: "a", MountPath: "/", LimitBytes: 1}}, "mount_path"},
		"no size":          {[]ManagedDir{{Name: "a", MountPath: "/a"}}, "limit_bytes"},
		"oversized":        {[]ManagedDir{{Name: "a", MountPath: "/a", LimitBytes: MaxManagedDirLimitBytes + 1}}, "limit_bytes"},
		"unknown retain":   {[]ManagedDir{{Name: "a", MountPath: "/a", LimitBytes: 1, Retain: "node"}}, "retain"},
		"same mount path":  {[]ManagedDir{{Name: "a", MountPath: "/a", LimitBytes: 1}, {Name: "b", MountPath: "/a", LimitBytes: 1}}, "overlapping"},
		"nested mount":     {[]ManagedDir{{Name: "a", MountPath: "/a", LimitBytes: 1}, {Name: "b", MountPath: "/a/b", LimitBytes: 1}}, "overlapping"},
		"nested the other": {[]ManagedDir{{Name: "a", MountPath: "/a/b", LimitBytes: 1}, {Name: "b", MountPath: "/a", LimitBytes: 1}}, "overlapping"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := NormalizeManagedDirs(test.dirs); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestManagedDirsColumnRoundTrip(t *testing.T) {
	if raw, err := EncodeManagedDirs(nil); err != nil || raw != "" {
		t.Fatalf("no directories = %q %v, want the blank column", raw, err)
	}
	if dirs, err := DecodeManagedDirs("  "); err != nil || dirs != nil {
		t.Fatalf("blank column = %v %v", dirs, err)
	}
	dirs := []ManagedDir{{Name: "pico-state", MountPath: "/pico", LimitBytes: 400, Retain: ManagedDirRetainDeployment}}
	raw, err := EncodeManagedDirs(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if raw != `[{"name":"pico-state","mount_path":"/pico","limit_bytes":400,"retain":"deployment"}]` {
		t.Fatalf("column = %s", raw)
	}
	back, err := DecodeManagedDirs(raw)
	if err != nil || !reflect.DeepEqual(back, dirs) {
		t.Fatalf("decoded = %+v %v", back, err)
	}
	if _, err := DecodeManagedDirs(`{"name":"x"}`); err == nil {
		t.Fatal("a column of another shape decoded")
	}
}

// Each declared directory becomes one managed mount the agent backs with a
// host directory: no source, the ceiling as a size hint, and the deployment's
// group, after the platform's other mounts.
func TestResolveLaunchAsksForAManagedMountPerDeclaredDirectory(t *testing.T) {
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: vLLMEngine{}, Params: Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1},
		ContainerOverrides: overridesWithMount("/opt/extra"),
		TemplateDirs:       []ManagedDir{{Name: "pico-state", MountPath: "/pico", LimitBytes: 400 << 30, Retain: ManagedDirRetainDeployment}},
		ManagedDirGroup:    "7-1700000000",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := proto.MountSpec{Target: "/pico", Purpose: proto.MountPurposeManagedDir, SizeHint: "429496729600",
		ManagedDir: &proto.ManagedDirRef{Group: "7-1700000000", Name: "pico-state"}}
	if len(launch.Mounts) != 2 || launch.Mounts[0].Target != "/opt/extra" || !reflect.DeepEqual(launch.Mounts[1], want) {
		t.Fatalf("mounts = %+v", launch.Mounts)
	}
	wire, err := json.Marshal(launch.Mounts[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(wire) != `{"target":"/pico","size_hint":"429496729600","purpose":"managed-dir","managed_dir":{"group":"7-1700000000","name":"pico-state"}}` {
		t.Fatalf("wire = %s", wire)
	}
}

// A directory on the same container path as an operator's extra mount is the
// conflict it is, not a mount silently shadowed.
func TestResolveLaunchRefusesAManagedDirectoryOnAnotherMountsTarget(t *testing.T) {
	_, err := ResolveLaunch(ResolveLaunchInput{
		Engine: vLLMEngine{}, Params: Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1},
		ContainerOverrides: overridesWithMount("/pico"),
		TemplateDirs:       []ManagedDir{{Name: "pico-state", MountPath: "/pico", LimitBytes: 1}},
	})
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorMountTargetDuplicate {
		t.Fatalf("err = %v, want %s", err, messages.ErrorMountTargetDuplicate)
	}
	if _, err := ResolveLaunch(ResolveLaunchInput{
		Engine: vLLMEngine{}, Params: Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1},
		TemplateDirs: []ManagedDir{{Name: "inside", MountPath: "/model/cache", LimitBytes: 1}},
	}); err == nil {
		t.Fatal("a directory inside the model mount was accepted")
	}
}

// A snapshot written before templates declared directories has none; one
// written with them keeps them through a decode/encode cycle.
func TestCatalogSelectionCarriesTheTemplatesDirectories(t *testing.T) {
	old := `{"template_id":3,"model_id":"m","spec_source":"upstream","spec_name":"s","engine":"vllm","image":"i","weight_provider":"huggingface","weight_repo_id":"org/m","gpus":{},"matched_by":"name"}`
	selection, err := DecodeCatalogSelection([]byte(old))
	if err != nil || selection.Dirs != nil || SnapshotDeclaresManagedDirs(old) {
		t.Fatalf("old snapshot dirs = %+v err=%v", selection.Dirs, err)
	}
	fresh := CatalogSelection{TemplateID: 3, ModelID: "m", Engine: "callable", Image: "i", GPUs: json.RawMessage(`{}`),
		Dirs: []ManagedDir{{Name: "pico-state", MountPath: "/pico", LimitBytes: 400, Retain: ManagedDirRetainDeployment}}}
	encoded, err := fresh.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"dirs":[{"name":"pico-state","mount_path":"/pico","limit_bytes":400,"retain":"deployment"}]`) {
		t.Fatalf("snapshot = %s", encoded)
	}
	decoded, err := DecodeCatalogSelection(encoded)
	if err != nil || !reflect.DeepEqual(decoded.Dirs, fresh.Dirs) || !SnapshotDeclaresManagedDirs(string(encoded)) {
		t.Fatalf("decoded dirs = %+v err=%v", decoded.Dirs, err)
	}
	if SnapshotDeclaresManagedDirs("not json") || SnapshotDeclaresManagedDirs("") {
		t.Fatal("an unreadable or empty snapshot declared directories")
	}
}

// The disk the node keeps managed directories on carries both kinds: the
// template's dirs and, when the deployment names a KV disk tier directory,
// that tier's ceiling (stated, else twice the weights).
func TestManagedDiskBytesCountsDirsAndTheKVDiskTier(t *testing.T) {
	if got := ManagedDiskBytes(400<<30, LaunchConfig{}, 10<<30); got != 400<<30 {
		t.Fatalf("dirs only = %d", got)
	}
	if got := ManagedDiskBytes(400<<30, LaunchConfig{SharedKVDir: "/kv"}, 10<<30); got != 420<<30 {
		t.Fatalf("dirs and derived KV ceiling = %d", got)
	}
	if got := ManagedDiskBytes(0, LaunchConfig{SharedKVDir: "/kv", KVDiskLimitGiB: 50}, 10<<30); got != 50<<30 {
		t.Fatalf("stated KV ceiling = %d", got)
	}
	if got := ManagedDiskBytes(-1, LaunchConfig{KVDiskLimitGiB: 50}, 10<<30); got != 0 {
		t.Fatalf("no tier directory, no dirs = %d", got)
	}
}
