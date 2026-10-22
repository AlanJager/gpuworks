package proto

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCleanStorageRel(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "",
		".":                  "",
		"/":                  "",
		"files/a.txt":        "files/a.txt",
		"/files//x/./y/":     "files/x/y",
		".gpuworks/apps/e/x": ".gpuworks/apps/e/x",
		"..hidden/model":     "..hidden/model",
		"weights..v2/last":   "weights..v2/last",
	} {
		got, err := CleanStorageRel(in)
		if err != nil || got != want {
			t.Errorf("CleanStorageRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "../x", "files/../../x", "files/..", "a\x00b", `files\x`} {
		if got, err := CleanStorageRel(in); err == nil {
			t.Errorf("CleanStorageRel(%q) = %q, want an error", in, got)
		}
	}
}

func TestStorageRelWritable(t *testing.T) {
	for rel, want := range map[string]bool{
		"":                         false,
		"files":                    false,
		"files/a":                  true,
		"files/a/b.tar":            true,
		"filesx/a":                 false,
		".gpuworks":                false,
		".gpuworks/apps":           false,
		".gpuworks/apps/evalscope": true,
		".gpuworks/incoming/cp/x":  false,
		".gpuworks/trash/x":        false,
		"Qwen/Qwen3-8B":            false,
	} {
		if got := StorageRelWritable(rel); got != want {
			t.Errorf("StorageRelWritable(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestStorageUploadStagingRel(t *testing.T) {
	if got := StorageUploadStagingRel("cp-a", "1f2e"); got != ".gpuworks/incoming/cp-a/upload-1f2e" {
		t.Fatalf("staging = %q", got)
	}
}

func TestStorageFilesErrorRoundTrip(t *testing.T) {
	err := NewStorageFilesError(StorageFilesErrExists, "target %s exists", "files/a")
	if err.Error() != "storage_files:exists: target files/a exists" {
		t.Fatalf("text = %q", err.Error())
	}
	back := ParseStorageFilesError(err.Error())
	if back.Code != StorageFilesErrExists || back.Detail != "target files/a exists" {
		t.Fatalf("parsed = %+v", back)
	}
	var target *StorageFilesError
	if !errors.As(error(err), &target) {
		t.Fatal("not a StorageFilesError")
	}
	if got := ParseStorageFilesError("rpc timeout"); got.Code != StorageFilesErrFailed || got.Detail != "rpc timeout" {
		t.Fatalf("plain text = %+v", got)
	}
	if got := ParseStorageFilesError("storage_files:bogus: x"); got.Code != StorageFilesErrFailed {
		t.Fatalf("unknown code = %+v", got)
	}
}

func TestStorageFilesWire(t *testing.T) {
	hdr := StreamHeader{Kind: StreamStorageUpload, StorageID: 3, Rel: "files/d.tar", Staging: ".gpuworks/incoming/cp/upload-1", Size: -1, Extract: StorageArchiveTar, Overwrite: true}
	raw, _ := json.Marshal(hdr)
	if string(raw) != `{"kind":"storage.upload","size":-1,"storage_id":3,"rel":"files/d.tar","staging":".gpuworks/incoming/cp/upload-1","extract":"tar","overwrite":true}` {
		t.Fatalf("header = %s", raw)
	}
	page := StorageFilesListResult{Entries: []StorageFileEntry{{Name: "a", Size: 1, ModTime: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)}}}
	raw, _ = json.Marshal(page)
	if string(raw) != `{"entries":[{"name":"a","dir":false,"size":1,"mtime":"2026-10-21T00:00:00Z"}]}` {
		t.Fatalf("page = %s", raw)
	}
	raw, _ = json.Marshal(StorageFilesStatus{State: StorageFilesDone, Bytes: 5, Files: 1})
	if string(raw) != `{"state":"done","bytes":5,"files":1}` {
		t.Fatalf("status = %s", raw)
	}
	raw, _ = json.Marshal(StorageDownloadHead{Dir: true, Size: -1, Name: "d"})
	if string(raw) != `{"dir":true,"size":-1,"name":"d"}` {
		t.Fatalf("head = %s", raw)
	}
}
