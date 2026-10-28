package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

// copyModelFiles writes a minimal model directory: weights, a config and a
// tokenizer, plus whatever metadata names the caller adds.
func copyModelFiles(t *testing.T, dir string, extra map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := map[string]string{
		"config.json":       `{"architectures":["BertForSequenceClassification"]}`,
		"tokenizer.json":    `{"model":{"type":"WordPiece"}}`,
		"model.safetensors": "weights-0123456789",
	}
	for name, content := range extra {
		body[name] = content
	}
	for name, content := range body {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCopyModelDirSkipsUnreadableSourceMarkers is the GWKS-142 regression: a
// ModelScope download in a shared library carries .msc/.mv, and a root_squashed
// NFS export serves a root:root 0600 marker as permission denied. The marker is
// source identity, not model content, so the copy must not open it and must not
// fail the whole model over it.
func TestCopyModelDirSkipsUnreadableSourceMarkers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "src")
	copyModelFiles(t, source, map[string]string{
		modelmetadata.ModelScopeMarker:        "",
		modelmetadata.ModelScopeVersionMarker: "Revision:master,CreatedAt:1756691498",
	})
	for _, name := range modelmetadata.SourceMarkers {
		if err := os.Chmod(filepath.Join(source, name), 0); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(t.TempDir(), "dst")
	r.copyModelDir(context.Background(), copyTask("markers", source), target, target)
	got := r.lastFile(t, "markers")
	if got.State != proto.MFPresent {
		t.Fatalf("an unreadable .msc/.mv failed the copy: %+v", got)
	}
	copyAssertByteIdentical(t, source, target, []string{"config.json", "tokenizer.json", "model.safetensors"})
	for _, name := range modelmetadata.SourceMarkers {
		if _, err := os.Stat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was copied into the model: %v", name, err)
		}
	}
}

// TestCopyModelDirNeverTouchesSourceMarkers pins the same behavior for every
// uid: a dangling .msc/.mv cannot be stat'ed or opened by anyone, so a copy
// that reaches the file at all fails. This case does not skip under root.
func TestCopyModelDirNeverTouchesSourceMarkers(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "src")
	copyModelFiles(t, source, nil)
	for _, name := range modelmetadata.SourceMarkers {
		// A dangling symlink fails os.Stat with ENOENT for root too.
		if err := os.Symlink(filepath.Join(source, "absent"), filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(t.TempDir(), "dst")
	r.copyModelDir(context.Background(), copyTask("dangling-markers", source), target, target)
	if got := r.lastFile(t, "dangling-markers"); got.State != proto.MFPresent {
		t.Fatalf("copy touched a source marker: %+v", got)
	}
	copyAssertByteIdentical(t, source, target, []string{"config.json", "tokenizer.json", "model.safetensors"})
}

// TestCopyModelDirStillFailsOnUnreadableWeights is the other half of GWKS-142:
// skipping tool metadata must not become a blanket tolerance of read errors.
// An unreadable weight file fails the deployment, because shipping an
// incomplete model to a GPU node fails later and more confusingly.
func TestCopyModelDirStillFailsOnUnreadableWeights(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "src")
	copyModelFiles(t, source, map[string]string{modelmetadata.ModelScopeMarker: ""})
	// The markers are skippable; the weights are not.
	if err := os.Chmod(filepath.Join(source, "model.safetensors"), 0); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "dst")
	r.copyModelDir(context.Background(), copyTask("unreadable-weight", source), target, target)
	got := r.lastFile(t, "unreadable-weight")
	if got.State != proto.MFFailed {
		t.Fatalf("an unreadable weight was tolerated: %+v", got)
	}
	if want := "model.safetensors"; !strings.Contains(got.Message, want) {
		t.Fatalf("failure does not name the weight file: %q", got.Message)
	}
}

// TestCopyModelDirFailsOnDanglingWeightLink is the uid-independent form of the
// case above: an unresolvable weight file is still a failure, never a skip.
func TestCopyModelDirFailsOnDanglingWeightLink(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "src")
	copyModelFiles(t, source, map[string]string{modelmetadata.ModelScopeMarker: ""})
	if err := os.Remove(filepath.Join(source, "model.safetensors")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "absent.safetensors"), filepath.Join(source, "model.safetensors")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "dst")
	r.copyModelDir(context.Background(), copyTask("dangling-weight", source), target, target)
	if got := r.lastFile(t, "dangling-weight"); got.State != proto.MFFailed {
		t.Fatalf("an unresolvable weight file was skipped: %+v", got)
	}
}

// TestListCopySourceSkipsEverySourceMarker guards against the two readers
// drifting apart: modelscan recognizes a ModelScope download by
// modelmetadata.SourceMarkers, so a copy must skip exactly that set. A name
// added there and not skipped here would fail every copy of such a library.
func TestListCopySourceSkipsEverySourceMarker(t *testing.T) {
	if len(modelmetadata.SourceMarkers) == 0 {
		t.Fatal("modelmetadata.SourceMarkers is empty; the set has no owner")
	}
	source := filepath.Join(t.TempDir(), "src")
	copyModelFiles(t, source, nil)
	for _, name := range modelmetadata.SourceMarkers {
		if err := os.WriteFile(filepath.Join(source, name), []byte("marker"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{modelCompleteMarker, copySourceMarker, "model.safetensors.part", "model.safetensors.part.state"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("task"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := listCopySource(source)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, f := range files {
		listed[f.rel] = true
	}
	if want := []string{"config.json", "model.safetensors", "tokenizer.json"}; len(files) != len(want) {
		t.Fatalf("listing = %v, want exactly %v", listed, want)
	}
	for _, f := range files {
		if copySkipName(f.rel) {
			t.Fatalf("%s is listed although copySkipName skips it", f.rel)
		}
	}
	for _, name := range append(append([]string{}, modelmetadata.SourceMarkers...), modelCompleteMarker, copySourceMarker) {
		if listed[name] {
			t.Fatalf("%s is task metadata but was listed", name)
		}
	}
}

// TestCopyResumeSeesTheSameFileSetAsTheListing pins the resume trap: the
// snapshot a resumed copy compares against is written from listCopySource, and
// the verification listing is listCopySource, so both sides must skip the same
// names. If one side skipped .msc and the other did not, every resume would
// decide the source had changed and restart forever.
func TestCopyResumeSeesTheSameFileSetAsTheListing(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	copyModelFiles(t, source, map[string]string{
		modelmetadata.ModelScopeMarker:        "",
		modelmetadata.ModelScopeVersionMarker: "Revision:master,CreatedAt:1756691498",
	})
	before, err := listCopySource(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareCopyResume(source, target, before); err != nil {
		t.Fatal(err)
	}
	// An interrupted copy left partial bytes and its snapshot behind.
	if err := os.WriteFile(filepath.Join(target, "model.safetensors.part"), []byte("weights-012"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Writing into the target is not a source change, and neither listing may
	// depend on anything that the target gained.
	after, err := listCopySource(source)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSource(before, after) {
		t.Fatalf("the source listing changed while its own copy ran:\nbefore=%v\nafter=%v", before, after)
	}
	// The snapshot on disk names exactly the listing, so prepareCopyResume finds
	// it unchanged and keeps the partial bytes instead of starting over.
	raw, err := os.ReadFile(filepath.Join(target, copySourceMarker))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot copySourceSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Files) != len(after) {
		t.Fatalf("snapshot names %d files, listing names %d: %+v", len(snapshot.Files), len(after), snapshot)
	}
	for i, entry := range snapshot.Files {
		if entry.Path != after[i].rel || entry.Size != after[i].size || !entry.Modified.Equal(after[i].modTime) {
			t.Fatalf("snapshot entry %d = %+v, listing = %+v", i, entry, after[i])
		}
		if copySkipName(entry.Path) {
			t.Fatalf("the resume snapshot names task metadata %q", entry.Path)
		}
	}
	// Re-running prepareCopyResume with the same listing is a no-op: it keeps
	// the partial file rather than deleting it and the copy starts from there.
	if err := prepareCopyResume(source, target, after); err != nil {
		t.Fatal(err)
	}
	partial, err := os.ReadFile(filepath.Join(target, "model.safetensors.part"))
	if err != nil || string(partial) != "weights-012" {
		t.Fatalf("the snapshot was invalidated: %q %v", partial, err)
	}
}

// TestCopyResumeOfAMarkerLibraryKeepsPartialBytes is the end-to-end form of the
// same property: a copy interrupted in a ModelScope library resumes in one
// attempt, so a marker the source skips can never restart it. The markers are
// dangling links, so this case fails for a copy that reaches them whatever uid
// the test runs as.
func TestCopyResumeOfAMarkerLibraryKeepsPartialBytes(t *testing.T) {
	r := newStorageTestRuntime(t)
	source, target := t.TempDir(), t.TempDir()
	weights := "weights-0123456789"
	copyModelFiles(t, source, nil)
	for _, name := range modelmetadata.SourceMarkers {
		if err := os.Symlink(filepath.Join(source, "absent"), filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}
	// An earlier attempt wrote the marker snapshot and part of the weights.
	files, err := listCopySource(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareCopyResume(source, target, files); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "model.safetensors.part"), []byte(weights[:8]), 0o644); err != nil {
		t.Fatal(err)
	}
	attempts := map[string]bool{}
	r.copyModelDir(context.Background(), copyTask("resume-markers", source), target, target)
	if got := r.lastFile(t, "resume-markers"); got.State != proto.MFPresent {
		t.Fatalf("resumed copy = %+v", got)
	}
	r.mu.Lock()
	for _, st := range r.files {
		if st.Preparation != nil {
			attempts[st.Preparation.Attempt] = true
		}
	}
	r.mu.Unlock()
	if len(attempts) != 1 {
		t.Fatalf("the copy restarted %d times; a resume must see one file set", len(attempts))
	}
	copyAssertByteIdentical(t, source, target, []string{"config.json", "tokenizer.json", "model.safetensors"})
}

// TestCopyKeepsDotDirectoriesSkipped keeps the older rule honest: a dot
// directory is tool metadata whatever it is called, and stays out of the copy.
func TestCopyKeepsDotDirectoriesSkipped(t *testing.T) {
	source := filepath.Join(t.TempDir(), "src")
	copyModelFiles(t, source, map[string]string{".msc-dir/meta": "skip me"})
	files, err := listCopySource(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.rel == filepath.Join(".msc-dir", "meta") {
			t.Fatal("a dot directory was listed")
		}
	}
}

// TestResumeOfALegacySnapshotThatNamedMarkersStartsOverOnce covers the upgrade:
// a copy interrupted by an agent that predates this fix left a snapshot naming
// .msc/.mv, whose partials were written including those files. The new listing
// no longer names them, so the snapshot is replaced once and the copy runs to
// the end — it must not keep deciding the source changed, and the marker bytes
// an old attempt wrote into the target must not survive as model content.
func TestResumeOfALegacySnapshotThatNamedMarkersStartsOverOnce(t *testing.T) {
	r := newStorageTestRuntime(t)
	source, target := t.TempDir(), t.TempDir()
	copyModelFiles(t, source, map[string]string{})
	for _, name := range modelmetadata.SourceMarkers {
		if err := os.WriteFile(filepath.Join(source, name), []byte("marker"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// What the older agent left behind: a snapshot that names the markers and
	// copies of them already in the target.
	legacy := copySourceSnapshot{Source: source, Files: []copySourceEntry{
		{Path: "config.json", Size: 52},
		{Path: modelmetadata.ModelScopeMarker, Size: 6},
		{Path: "model.safetensors", Size: 19},
	}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, copySourceMarker), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range modelmetadata.SourceMarkers {
		if err := os.WriteFile(filepath.Join(target, name), []byte("marker"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.copyModelDir(context.Background(), copyTask("legacy-markers", source), target, target)
	if got := r.lastFile(t, "legacy-markers"); got.State != proto.MFPresent {
		t.Fatalf("a legacy snapshot naming .msc did not converge: %+v", got)
	}
	for _, name := range modelmetadata.SourceMarkers {
		if _, err := os.Stat(filepath.Join(target, name)); !os.IsNotExist(err) {
			t.Fatalf("the old attempt's %s survived as model content: %v", name, err)
		}
	}
	copyAssertByteIdentical(t, source, target, []string{"config.json", "tokenizer.json", "model.safetensors"})
	if _, ok := validCompletionMarker(target, copyTask("legacy-markers", source)); !ok {
		t.Fatal("no valid completion marker after the copy")
	}
}

// copyAssertByteIdentical fails unless every named file is present in target
// with the source's exact bytes.
func copyAssertByteIdentical(t *testing.T, source, target string, names []string) {
	t.Helper()
	for _, name := range names {
		want, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("read copied %s: %v", name, err)
		}
		if fmt.Sprintf("%x", sha256.Sum256(got)) != fmt.Sprintf("%x", sha256.Sum256(want)) {
			t.Fatalf("%s did not copy byte for byte: %q != %q", name, got, want)
		}
	}
}
