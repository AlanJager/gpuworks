package app

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The layout names storagemount repeats (it cannot import proto) must be the
// ones the server and the rest of the agent use.
func TestStorageLayoutNamesMatchProto(t *testing.T) {
	for name, pair := range map[string][2]string{
		"meta":           {storagemount.MetaDirName, proto.StorageMetaDir},
		"trash":          {storagemount.TrashDirName, proto.StorageTrashDir},
		"control planes": {storagemount.ControlPlanesDirName, proto.StorageControlPlanesDir},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s directory: storagemount says %q, proto says %q", name, pair[0], pair[1])
		}
	}
	root := t.TempDir()
	if got, want := storagemount.TrashPath(root, "m", time.Time{}), filepath.Join(root, proto.StorageMetaDir, proto.StorageTrashDir); filepath.Dir(got) != want {
		t.Errorf("trash path %q is not under %q", got, want)
	}
}

func TestResolveStorageTarget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	abs := filepath.Join(t.TempDir(), "own", "copy")
	if got, err := resolveStorageTarget(root, storageTarget{Rel: "Qwen/Qwen3-8B"}); err != nil || got != filepath.Join(root, "Qwen", "Qwen3-8B") {
		t.Fatalf("relative = %q, %v", got, err)
	}
	if got, err := resolveStorageTarget(root, storageTarget{Abs: abs}); err != nil || got != abs {
		t.Fatalf("a local storage's absolute path is used as is: %q, %v", got, err)
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/../../x", "/", "/etc"} {
		if got, err := resolveStorageTarget(root, storageTarget{Rel: bad}); err == nil {
			t.Errorf("relative target %q resolved to %q", bad, got)
		}
	}
	for name, target := range map[string]storageTarget{
		"neither":             {},
		"both":                {Abs: abs, Rel: "Qwen/Qwen3-8B"},
		"a relative Path":     {Abs: "Qwen/Qwen3-8B"},
		"an absolute RelPath": {Rel: abs},
	} {
		if got, err := resolveStorageTarget(root, target); err == nil {
			t.Errorf("%s resolved to %q", name, got)
		}
	}
}

// A task names its target in Path (absolute) or RelPath (below the storage
// root); only a server that predates the two sends TargetPath alone, and the
// agent then tells the kinds apart the way it always did.
func TestTaskTargetReadsTheExplicitFieldsAndTheLegacyOne(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "own", "copy")
	cases := []struct {
		name string
		task proto.ModelFileTask
		want storageTarget
	}{
		{"Path", proto.ModelFileTask{Path: abs}, storageTarget{Abs: abs}},
		{"RelPath", proto.ModelFileTask{RelPath: "Qwen/Qwen3-8B"}, storageTarget{Rel: "Qwen/Qwen3-8B"}},
		{"the new fields win over TargetPath", proto.ModelFileTask{RelPath: "new", TargetPath: abs}, storageTarget{Rel: "new"}},
		{"legacy absolute", proto.ModelFileTask{TargetPath: abs}, storageTarget{Abs: abs}},
		{"legacy relative", proto.ModelFileTask{TargetPath: "Qwen/Qwen3-8B"}, storageTarget{Rel: "Qwen/Qwen3-8B"}},
		{"nothing", proto.ModelFileTask{}, storageTarget{}},
	}
	for _, tc := range cases {
		if got := taskTarget(tc.task); got != tc.want {
			t.Errorf("%s: target = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	// The same tasks resolve to the same directory whichever form carried them.
	root := filepath.Join(t.TempDir(), "root")
	for _, pair := range [][2]proto.ModelFileTask{
		{{Path: abs}, {TargetPath: abs}},
		{{RelPath: "Qwen/Qwen3-8B"}, {TargetPath: "Qwen/Qwen3-8B"}},
	} {
		explicit, err1 := resolveStorageTarget(root, taskTarget(pair[0]))
		legacy, err2 := resolveStorageTarget(root, taskTarget(pair[1]))
		if err1 != nil || err2 != nil || explicit != legacy {
			t.Errorf("explicit %q (%v) != legacy %q (%v)", explicit, err1, legacy, err2)
		}
	}
}

func TestStagingWorkDirStaysInTheIncomingDirectory(t *testing.T) {
	root := t.TempDir()
	good := proto.StagingRelPath("cp-a", "Qwen/Qwen3-8B")
	if got, err := stagingWorkDir(root, good); err != nil || got != filepath.Join(root, good) {
		t.Fatalf("staging = %q, %v", got, err)
	}
	for _, bad := range []string{
		"", ".", "/", "..", "../x", "/etc", filepath.Join(root, good),
		".gpuworks", ".gpuworks/incoming", ".gpuworks/incoming/cp-a", ".gpuworks/incoming/../trash/x/y",
		".gpuworks/trash/cp-a/m", ".gpuworks/control-planes/cp-a/x", "Qwen/Qwen3-8B", ".gpuworks/incoming/cp-a/../../..",
	} {
		if got, err := stagingWorkDir(root, bad); err == nil {
			t.Errorf("staging %q was accepted as %q", bad, got)
		}
	}
}

// A task whose staging or target path would reach the storage itself is
// refused and reported, and nothing on the storage is touched: the staging
// directory is deleted when the install finds the model already in place.
func TestModelFileTaskRefusesPathsOutsideTheStorage(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	canary := filepath.Join(shared, "Keep", "me")
	writeModel(t, canary, "precious")
	otherPlane := filepath.Join(shared, proto.StagingRelPath("cp-b", "Other/model"))
	writeModel(t, otherPlane, "another control plane's work")
	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "weights")
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)

	tasks := map[string]proto.ModelFileTask{
		"staging is the root":        {Key: "f1", TargetPath: "Qwen/a", Staging: "."},
		"staging is the incoming":    {Key: "f2", TargetPath: "Qwen/a", Staging: ".gpuworks/incoming"},
		"staging escapes":            {Key: "f3", TargetPath: "Qwen/a", Staging: "../elsewhere"},
		"staging is absolute":        {Key: "f4", TargetPath: "Qwen/a", Staging: shared},
		"staging is a model":         {Key: "f5", TargetPath: "Qwen/a", Staging: "Keep/me"},
		"staging is the plane's own": {Key: "f6", TargetPath: "Qwen/a", Staging: ".gpuworks/incoming/cp-a"},
		"target is the root":         {Key: "f7", TargetPath: ".", Staging: proto.StagingRelPath("cp-a", "x")},
		"target escapes":             {Key: "f8", TargetPath: "../x", Staging: proto.StagingRelPath("cp-a", "x")},
		"target is empty":            {Key: "f9", TargetPath: ""},
	}
	for name, task := range tasks {
		task.StorageID, task.DesiredState = 2, "present"
		task.Acquisition, task.SourcePath = proto.ModelAcquireCopy, source
		task.Source, task.SourceID = "local", "Qwen/a"
		r.reconcileModelFile(context.Background(), task)
		got := r.lastFile(t, task.Key)
		if got.State != proto.MFFailed || !strings.Contains(got.Message, "path") {
			t.Errorf("%s: status = %+v", name, got)
		}
	}
	for _, dir := range []string{canary, otherPlane} {
		if _, err := os.Stat(filepath.Join(dir, "model.safetensors")); err != nil {
			t.Errorf("%s was touched: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(shared, "Qwen")); !os.IsNotExist(err) {
		t.Errorf("a refused task still wrote a model: %v", err)
	}
}

// The receive side of a peer sync must write where the target storage says,
// not under the default models root.
func TestModelSyncReceivesIntoTheTargetStorage(t *testing.T) {
	r := newStorageTestRuntime(t)
	fast := filepath.Join(t.TempDir(), "fast-disk")
	r.applyStorages([]proto.StorageTask{
		{ID: 1, Name: "default", Type: proto.StorageLocal, Path: r.ModelsRoot()},
		{ID: 5, Name: "fast", Type: proto.StorageLocal, Path: fast},
	})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)

	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "weights")
	var archive bytes.Buffer
	if err := (&NodeRuntime{}).StreamModelArchive(context.Background(), source, &archive); err != nil {
		t.Fatal(err)
	}

	listenAny, err := r.rpcModelSyncListen(mustRaw(t, proto.ModelSyncListenParams{
		TransferID: "t1", Key: "f9", TargetPath: "Qwen/Qwen3-8B", StorageID: 5, Token: "tok", BindIP: "127.0.0.1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	listen := listenAny.(proto.ModelSyncListenResult)
	if _, err := agentmodelsync.Send(context.Background(), listen.Endpoints, net.ParseIP("127.0.0.1"), "tok", listen.Fingerprint, func(w io.Writer) error {
		_, err := w.Write(archive.Bytes())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := r.waitFile(t, "f9", proto.MFPresent, proto.MFFailed)
	want := filepath.Join(fast, "Qwen", "Qwen3-8B")
	if got.State != proto.MFPresent || got.Path != want {
		t.Fatalf("sync into the second local storage = %+v, want present at %s", got, want)
	}
	if !isModelPresent(want) {
		t.Fatalf("the model is not under the target storage: %s", want)
	}
	if _, err := os.Stat(filepath.Join(r.ModelsRoot(), "Qwen")); !os.IsNotExist(err) {
		t.Fatalf("the archive also landed under the default models root: %v", err)
	}
}

func TestReceiveModelArchiveRefusesAnEscapingPath(t *testing.T) {
	root := t.TempDir()
	n := &NodeRuntime{modelsDir: filepath.Join(root, "models")}
	for _, bad := range []string{"../escape", ".", ""} {
		if err := n.ReceiveModelArchive(context.Background(), bad, strings.NewReader("")); err == nil {
			t.Errorf("receive at %q accepted", bad)
		}
	}
}
