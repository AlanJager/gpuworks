package storagefiles

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

var bg = context.Background()

func names(entries []proto.StorageFileEntry) string {
	var out []string
	for _, e := range entries {
		kind := "f"
		switch {
		case e.Symlink:
			kind = "l"
		case e.Dir:
			kind = "d"
		}
		out = append(out, e.Name+":"+kind)
	}
	return strings.Join(out, ",")
}

func TestListEmptyRootShowsTheWritableRegionsAsDirectories(t *testing.T) {
	e := newEnv(t)
	res, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Entries); got != ".gpuworks:d,files:d" || res.NextCursor != "" {
		t.Fatalf("root listing = %s next=%q", got, res.NextCursor)
	}
	for _, rel := range []string{".gpuworks", "files", ".gpuworks/apps"} {
		res, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: rel})
		if err != nil {
			t.Fatalf("listing the missing region directory %q: %v", rel, err)
		}
		want := ""
		if rel == ".gpuworks" {
			want = "apps:d"
		}
		if got := names(res.Entries); got != want || res.Entries == nil {
			t.Fatalf("listing %q = %q (nil=%v), want %q", rel, got, res.Entries == nil, want)
		}
	}
	// Anything else that is missing is not found.
	_, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "models/llama"})
	wantCode(t, err, proto.StorageFilesErrNotFound)
	_, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: ".gpuworks/trash"})
	wantCode(t, err, proto.StorageFilesErrNotFound)

	// An existing .gpuworks keeps its own entries and gains the virtual apps.
	writeFile(t, e.path(".gpuworks/control-planes/cp1.json"), "{}")
	writeFile(t, e.path("models/x/config.json"), "{}")
	res, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Entries); got != ".gpuworks:d,files:d,models:d" {
		t.Fatalf("root listing = %s", got)
	}
	res, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: ".gpuworks"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Entries); got != "apps:d,control-planes:d" {
		t.Fatalf(".gpuworks listing = %s", got)
	}
	if res.Entries[0].ModTime.IsZero() == false {
		t.Fatalf("a virtual entry has a modification time: %+v", res.Entries[0])
	}
}

func TestListReadsWithLstatSortsAndPages(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/b.txt"), "hello")
	writeFile(t, e.path("files/a/inner.txt"), "x")
	symlink(t, e.outside, e.path("files/link-out"))
	symlink(t, e.path("files/a"), e.path("files/link-in"))
	writeFile(t, e.path("files/C.txt"), "")
	res, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "files"})
	if err != nil {
		t.Fatal(err)
	}
	// Byte order: upper case first; links are links, never what they point to.
	if got := names(res.Entries); got != "C.txt:f,a:d,b.txt:f,link-in:l,link-out:l" {
		t.Fatalf("listing = %s", got)
	}
	for _, en := range res.Entries {
		switch en.Name {
		case "b.txt":
			if en.Size != 5 || en.ModTime.IsZero() || en.ModTime.Location().String() != "UTC" {
				t.Fatalf("file entry = %+v", en)
			}
		case "a":
			if en.Size != 0 {
				t.Fatalf("directory entry has a size: %+v", en)
			}
		case "link-in":
			if en.Dir {
				t.Fatalf("a link to a directory was followed: %+v", en)
			}
		}
	}
	page1, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "files", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(page1.Entries); got != "C.txt:f,a:d" || page1.NextCursor != "a" {
		t.Fatalf("page 1 = %s next=%q", got, page1.NextCursor)
	}
	page2, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "files", Limit: 2, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(page2.Entries); got != "b.txt:f,link-in:l" || page2.NextCursor != "link-in" {
		t.Fatalf("page 2 = %s next=%q", got, page2.NextCursor)
	}
	page3, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "files", Limit: 2, Cursor: page2.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(page3.Entries); got != "link-out:l" || page3.NextCursor != "" {
		t.Fatalf("page 3 = %s next=%q", got, page3.NextCursor)
	}
	// A link inside the root may be browsed through.
	res, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "files/link-in"})
	if err != nil || names(res.Entries) != "inner.txt:f" {
		t.Fatalf("listing through an inner link = %s, %v", names(res.Entries), err)
	}
}

func TestListClampsTheLimit(t *testing.T) {
	e := newEnv(t)
	dir := e.path("files/many")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < proto.StorageFilesListMaxLimit+3; i++ {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("f%04d", i)))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	for _, limit := range []int{0, -1, proto.StorageFilesListMaxLimit + 1, 100000} {
		res, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "files/many", Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Entries) != proto.StorageFilesListMaxLimit || res.NextCursor != fmt.Sprintf("f%04d", proto.StorageFilesListMaxLimit-1) {
			t.Fatalf("limit %d: %d entries next=%q", limit, len(res.Entries), res.NextCursor)
		}
	}
}

func TestListRefusesPathsThatLeaveTheStorage(t *testing.T) {
	e := newEnv(t)
	symlink(t, e.outside, e.path("files/out"))
	symlink(t, e.outside, e.path("models/escape"))
	symlink(t, "/nonexistent-target-for-test", e.path("files/dangling"))
	writeFile(t, e.path("files/plain.txt"), "x")
	for _, rel := range []string{"files/out", "files/out/sub", "models/escape", "files/dangling/x"} {
		_, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: rel})
		wantCode(t, err, proto.StorageFilesErrPathInvalid)
	}
	for _, rel := range []string{"../outside", "files/../../outside", `files\x`, "files/\x00"} {
		_, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: rel})
		wantCode(t, err, proto.StorageFilesErrPathInvalid)
	}
	_, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "files/plain.txt"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	_, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "files/plain.txt/x"})
	wantCode(t, err, proto.StorageFilesErrNotFound)
	// "/" and "." name the root.
	for _, rel := range []string{"/", ".", "./files/.."[:7]} {
		if _, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: rel}); err != nil {
			t.Fatalf("listing %q: %v", rel, err)
		}
	}
}

func TestStorageStateGatesEveryOperation(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/a.txt"), "x")
	// Read-only here: reads work, writes are not_writable.
	e.r.Writable = false
	if _, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1, Path: "files"}); err != nil {
		t.Fatalf("listing a read-only storage: %v", err)
	}
	_, err := e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/d"})
	wantCode(t, err, proto.StorageFilesErrNotWritable)
	_, err = e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/a.txt", To: "files/b.txt"})
	wantCode(t, err, proto.StorageFilesErrNotWritable)
	_, err = e.svc.Delete(bg, proto.StorageFilesDeleteParams{StorageID: 1, Path: "files/a.txt"})
	wantCode(t, err, proto.StorageFilesErrNotWritable)
	// Not ready: nothing works.
	e.r = Root{Dir: e.root, Ready: false, Writable: true}
	_, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1})
	wantCode(t, err, proto.StorageFilesErrNotWritable)
	// Unknown storage, or a root the caller cannot resolve.
	_, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 2})
	wantCode(t, err, proto.StorageFilesErrNotWritable)
	e.r = Root{Dir: filepath.Join(e.root, "gone"), Ready: true, Writable: true}
	_, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1})
	wantCode(t, err, proto.StorageFilesErrNotWritable)
	e.r = Root{Dir: " ", Ready: true, Writable: true}
	_, err = e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1})
	wantCode(t, err, proto.StorageFilesErrNotWritable)
	if _, err := New(Options{}).List(bg, proto.StorageFilesListParams{StorageID: 1}); codeOf(t, err) != proto.StorageFilesErrNotWritable {
		t.Fatalf("a service without storages: %v", err)
	}
	if !exists(e.path("files/a.txt")) {
		t.Fatal("a refused operation changed the storage")
	}
}

func TestGuardFailuresPropagateAsFailed(t *testing.T) {
	e := newEnv(t)
	e.doErr = storagemount.ErrUnresponsive
	_, err := e.svc.List(bg, proto.StorageFilesListParams{StorageID: 1})
	wantCode(t, err, proto.StorageFilesErrFailed)
	if !strings.Contains(err.Error(), "did not return in time") {
		t.Fatalf("detail lost: %v", err)
	}
	_, err = e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/x"})
	wantCode(t, err, proto.StorageFilesErrFailed)
	if exists(e.path("files")) {
		t.Fatal("a call the guard refused still ran")
	}
	if got := strings.Join(e.ops, ","); got != "files.list,files.mkdir" {
		t.Fatalf("guarded ops = %s", got)
	}
	// Without a guard the call runs directly.
	direct := New(Options{Root: func(uint) (Root, error) { return Root{Dir: e.root, Ready: true, Writable: true}, nil }})
	if _, err := direct.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/x"}); err != nil {
		t.Fatal(err)
	}
	// Plain errors get a code too.
	if fe := asFilesError(os.ErrExist); fe.Code != proto.StorageFilesErrExists {
		t.Fatalf("ErrExist -> %s", fe.Code)
	}
	if fe := asFilesError(fmt.Errorf("wrapped: %w", os.ErrNotExist)); fe.Code != proto.StorageFilesErrNotFound {
		t.Fatalf("ErrNotExist -> %s", fe.Code)
	}
	if asFilesError(nil) != nil {
		t.Fatal("nil error got a code")
	}
}

func TestMkdirCreatesInsideTheRegionsOnly(t *testing.T) {
	e := newEnv(t)
	res, err := e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/a/b"})
	if err != nil || res.Path != "files/a/b" {
		t.Fatalf("mkdir = %+v, %v", res, err)
	}
	if info, err := os.Stat(e.path("files/a/b")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
		t.Fatalf("created directory: %v %v", info, err)
	}
	if _, err := e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "/files/a/b/"}); err != nil {
		t.Fatalf("an existing directory: %v", err)
	}
	if _, err := e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: ".gpuworks/apps/eval/out"}); err != nil {
		t.Fatalf("the apps region: %v", err)
	}
	writeFile(t, e.path("files/file"), "x")
	_, err = e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/file"})
	wantCode(t, err, proto.StorageFilesErrExists)
	_, err = e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/file/sub"})
	wantCode(t, err, proto.StorageFilesErrExists)

	for _, rel := range []string{"", "files", ".gpuworks", ".gpuworks/apps", "models/x", ".gpuworks/trash/x", ".gpuworks/incoming/x"} {
		_, err := e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: rel})
		wantCode(t, err, proto.StorageFilesErrRegionReadonly)
	}
	_, err = e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/../models"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)

	// A link inside the region that leads to a model directory does not make
	// the model writable; one that leaves the storage is refused outright.
	writeFile(t, e.path("models/llama/config.json"), "{}")
	symlink(t, e.path("models/llama"), e.path("files/to-model"))
	_, err = e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/to-model/new"})
	wantCode(t, err, proto.StorageFilesErrRegionReadonly)
	symlink(t, e.outside, e.path("files/out"))
	_, err = e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/out/new"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	symlink(t, filepath.Join(e.outside, "not-yet"), e.path("files/dangling"))
	_, err = e.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/dangling/new"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	if exists(e.path("models/llama/new")) || exists(filepath.Join(e.outside, "new")) || exists(filepath.Join(e.outside, "not-yet")) {
		t.Fatal("mkdir wrote through a link")
	}
	// A region directory that is itself a link out of the storage.
	e2 := newEnv(t)
	symlink(t, e2.outside, e2.path("files"))
	_, err = e2.svc.Mkdir(bg, proto.StorageFilesMkdirParams{StorageID: 1, Path: "files/x"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	if exists(filepath.Join(e2.outside, "x")) {
		t.Fatal("mkdir followed the region link out of the storage")
	}
}

func TestRenameWithinTheRegions(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/a.txt"), "A")
	writeFile(t, e.path("files/b.txt"), "B")
	res, err := e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/a.txt", To: ".gpuworks/apps/x/a.txt"})
	if err != nil || res.Path != ".gpuworks/apps/x/a.txt" {
		t.Fatalf("rename = %+v, %v", res, err)
	}
	if readFile(t, e.path(".gpuworks/apps/x/a.txt")) != "A" || exists(e.path("files/a.txt")) {
		t.Fatal("rename did not move the file")
	}
	_, err = e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: ".gpuworks/apps/x/a.txt", To: "files/b.txt"})
	wantCode(t, err, proto.StorageFilesErrExists)
	if readFile(t, e.path("files/b.txt")) != "B" {
		t.Fatal("rename replaced an existing target")
	}
	_, err = e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/missing", To: "files/c"})
	wantCode(t, err, proto.StorageFilesErrNotFound)
	if err := os.MkdirAll(e.path("files/dir/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/dir", To: "files/dir/sub/dir"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	_, err = e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/dir", To: "files/dir"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	_, err = e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/b.txt", To: "files/b.txt/c"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)

	// Region directories cannot move, nothing moves out of or into the read-only part.
	writeFile(t, e.path("models/m/config.json"), "{}")
	for _, p := range [][2]string{{"files", "files/x"}, {".gpuworks/apps", "files/apps"}, {"files/b.txt", "models/b.txt"},
		{"models/m", "files/m"}, {"files/b.txt", "files"}, {"files/b.txt", ""}} {
		_, err := e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: p[0], To: p[1]})
		wantCode(t, err, proto.StorageFilesErrRegionReadonly)
	}
	// Into a model through a link inside the region.
	symlink(t, e.path("models/m"), e.path("files/to-model"))
	_, err = e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/b.txt", To: "files/to-model/b.txt"})
	wantCode(t, err, proto.StorageFilesErrRegionReadonly)
	// A link that points out of the storage is an entry like any other: it can
	// be renamed (the link moves, its target is untouched) ...
	symlink(t, e.outside, e.path("files/out"))
	if _, err := e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/out", To: "files/out2"}); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(e.path("files/out2")); err != nil || target != e.outside || readFile(t, filepath.Join(e.outside, "secret.txt")) != "secret" {
		t.Fatalf("renamed link -> %q, %v", target, err)
	}
	// ... but nothing can be moved through it.
	_, err = e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/b.txt", To: "files/out2/b.txt"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	_, err = e.svc.Rename(bg, proto.StorageFilesRenameParams{StorageID: 1, From: "files/out2/secret.txt", To: "files/stolen.txt"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	if exists(filepath.Join(e.outside, "b.txt")) || !exists(filepath.Join(e.outside, "secret.txt")) {
		t.Fatal("rename crossed a link out of the storage")
	}
}

func TestDeleteMovesIntoTheTrashAndEmptiesIt(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/d/a.txt"), "A")
	writeFile(t, e.path(".gpuworks/apps/eval/r.json"), "{}")
	res, err := e.svc.Delete(bg, proto.StorageFilesDeleteParams{StorageID: 1, Path: "files/d"})
	if err != nil || res.Path != "files/d" {
		t.Fatalf("delete = %+v, %v", res, err)
	}
	if exists(e.path("files/d")) {
		t.Fatal("the directory is still there")
	}
	eventually(t, "the trash emptied", e.trashEmpty)
	if _, err := e.svc.Delete(bg, proto.StorageFilesDeleteParams{StorageID: 1, Path: ".gpuworks/apps/eval"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Delete(bg, proto.StorageFilesDeleteParams{StorageID: 1, Path: "files/d"})
	wantCode(t, err, proto.StorageFilesErrNotFound)
	writeFile(t, e.path("models/m/config.json"), "{}")
	for _, rel := range []string{"", "files", ".gpuworks", ".gpuworks/apps", "models/m", ".gpuworks/trash", "files/../models"} {
		_, err := e.svc.Delete(bg, proto.StorageFilesDeleteParams{StorageID: 1, Path: rel})
		if rel == "files/../models" {
			wantCode(t, err, proto.StorageFilesErrPathInvalid)
			continue
		}
		wantCode(t, err, proto.StorageFilesErrRegionReadonly)
	}
	if !exists(e.path("models/m/config.json")) {
		t.Fatal("delete touched a model")
	}
	// Deleting a link removes the link, never what it points to.
	symlink(t, e.outside, e.path("files/out"))
	if _, err := e.svc.Delete(bg, proto.StorageFilesDeleteParams{StorageID: 1, Path: "files/out"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the trash emptied", e.trashEmpty)
	if exists(e.path("files/out")) || readFile(t, filepath.Join(e.outside, "secret.txt")) != "secret" {
		t.Fatal("deleting a link reached its target")
	}
	// Nothing below such a link can be deleted.
	symlink(t, e.outside, e.path("files/out"))
	_, err = e.svc.Delete(bg, proto.StorageFilesDeleteParams{StorageID: 1, Path: "files/out/secret.txt"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	if !exists(filepath.Join(e.outside, "secret.txt")) {
		t.Fatal("delete followed a link out of the storage")
	}
}

func TestDeleteRefusesATrashThatIsALink(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.path("files/a.txt"), "A")
	symlink(t, e.outside, e.path(".gpuworks/trash"))
	_, err := e.svc.Delete(bg, proto.StorageFilesDeleteParams{StorageID: 1, Path: "files/a.txt"})
	wantCode(t, err, proto.StorageFilesErrPathInvalid)
	if !exists(e.path("files/a.txt")) {
		t.Fatal("the file left although the trash is a link")
	}
	ents, _ := os.ReadDir(e.outside)
	if len(ents) != 1 {
		t.Fatalf("something was moved out of the storage: %v", ents)
	}
}

func TestDeleteReportsARenameFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newEnv(t)
	writeFile(t, e.path("files/locked/a.txt"), "A")
	if err := os.Chmod(e.path("files/locked"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.path("files/locked"), 0o755) })
	_, err := e.svc.Delete(bg, proto.StorageFilesDeleteParams{StorageID: 1, Path: "files/locked/a.txt"})
	wantCode(t, err, proto.StorageFilesErrFailed)
	if !exists(e.path("files/locked/a.txt")) {
		t.Fatal("a failed delete lost the file")
	}
}
