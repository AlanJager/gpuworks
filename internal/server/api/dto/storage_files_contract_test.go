package dto

import (
	"encoding/json"
	"testing"
	"time"
)

func roundTrip[T any](t *testing.T, wire string) T {
	t.Helper()
	var decoded T
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != wire {
		t.Fatalf("round trip = %s\nwant        %s", encoded, wire)
	}
	return decoded
}

// A listing always carries every key: the flags as false, items as [] and
// next_cursor as "" on the last page, so a client never meets a missing key.
func TestStorageFileListWireContract(t *testing.T) {
	const wire = `{"path":"files","node_uuid":"n1","writable":true,"items":[{"name":"a.txt","path":"files/a.txt","dir":false,"symlink":false,"size":12,"mtime":"2026-10-21T08:00:00Z","writable":true},{"name":"ln","path":"files/ln","dir":false,"symlink":true,"size":4,"mtime":"2026-10-21T09:00:00Z","writable":false}],"next_cursor":"ln"}`
	list := roundTrip[StorageFileList](t, wire)
	if len(list.Items) != 2 || !list.Items[1].Symlink || list.Items[0].Size != 12 || !list.Items[0].ModTime.Equal(time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("decoded = %+v", list)
	}
	encoded, err := json.Marshal(StorageFileList{Items: []StorageFileItem{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"path":"","node_uuid":"","writable":false,"items":[],"next_cursor":""}` {
		t.Fatalf("empty listing = %s", encoded)
	}
}

func TestStorageFileRequestsAndResultsWireContract(t *testing.T) {
	if got := roundTrip[StorageFileMkdirRequest](t, `{"node_uuid":"n1","path":"files/d"}`); got.Path != "files/d" || got.NodeUUID != "n1" {
		t.Fatalf("mkdir = %+v", got)
	}
	if got := roundTrip[StorageFileRenameRequest](t, `{"node_uuid":"","from":"files/a","to":"files/b"}`); got.From != "files/a" || got.To != "files/b" {
		t.Fatalf("rename = %+v", got)
	}
	if got := roundTrip[StorageFilePath](t, `{"path":"files/b"}`); got.Path != "files/b" {
		t.Fatalf("path = %+v", got)
	}
	if got := roundTrip[StorageFileUploadResult](t, `{"path":"files/a.txt","bytes":12,"files":1}`); got.Bytes != 12 || got.Files != 1 {
		t.Fatalf("upload = %+v", got)
	}
	encoded, _ := json.Marshal(StorageFileUploadResult{Path: "files/empty"})
	if string(encoded) != `{"path":"files/empty","bytes":0,"files":0}` {
		t.Fatalf("empty upload = %s", encoded)
	}
}
