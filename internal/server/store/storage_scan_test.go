package store

import (
	"fmt"
	"sort"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestApplyStorageScanMarksMissingRestoresAndResizes(t *testing.T) {
	f := newStorageFixture(t)
	st := f.st
	rows := map[string]*ModelFile{}
	for i, c := range []struct{ path, state string }{
		{"Qwen/deleted", proto.MFPresent},
		{"Qwen/back", proto.MFMissing},
		{"Qwen/grown", proto.MFPresent},
		{"Qwen/unreadable", proto.MFPresent},
		{"Qwen/writing", proto.MFDownloading},
		{"Z/later-page", proto.MFPresent},
		{"deep/a/b/c/d/e", proto.MFPresent},
		{"Qwen/grown/inner", proto.MFPresent},
		{"Qwen/stat-says-there", proto.MFPresent},
	} {
		m, err := st.UpsertModel(&Model{Source: "modelscope", SourceID: fmt.Sprintf("Qwen/m%d", i), Name: fmt.Sprintf("m%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		mf, err := st.UpsertModelFile(&ModelFile{ModelID: m.ID, StorageID: f.nfs.ID, Path: c.path, State: c.state, SizeBytes: 10, Downloaded: 10})
		if err != nil {
			t.Fatal(err)
		}
		rows[c.path] = mf
	}
	// The same directory in another storage is not this scan's business.
	other, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, Path: "Qwen/deleted", State: proto.MFPresent, SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}

	var asked []string
	got, err := st.ApplyStorageScan(f.nfs.ID, ScanSighting{
		Covers: func(rel string) bool { return rel != "Z/later-page" },
		Found:  map[string]int64{"Qwen/back": 10, "Qwen/grown": 25},
		Unsure: map[string]bool{"Qwen/unreadable": true, "deep/a/b": true},
		Gone:   func(rel string) bool { asked = append(asked, rel); return rel == "Qwen/deleted" },
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(asked)
	if len(asked) != 2 || asked[0] != "Qwen/deleted" || asked[1] != "Qwen/stat-says-there" {
		t.Fatalf("confirmed %v, want only the present files the page did not find", asked)
	}
	if got != (ScanChanges{Missing: 1, Restored: 1, Resized: 1}) {
		t.Fatalf("changes = %+v", got)
	}
	want := map[string]struct {
		state string
		size  int64
	}{
		"Qwen/deleted":         {proto.MFMissing, 10},
		"Qwen/back":            {proto.MFPresent, 10},
		"Qwen/grown":           {proto.MFPresent, 25},
		"Qwen/unreadable":      {proto.MFPresent, 10},
		"Qwen/writing":         {proto.MFDownloading, 10},
		"Z/later-page":         {proto.MFPresent, 10},
		"deep/a/b/c/d/e":       {proto.MFPresent, 10}, // below a directory the scan did not enter
		"Qwen/grown/inner":     {proto.MFPresent, 10}, // inside another model directory
		"Qwen/stat-says-there": {proto.MFPresent, 10},
	}
	for path, w := range want {
		mf, err := st.ModelFileByID(rows[path].ID)
		if err != nil {
			t.Fatal(err)
		}
		if mf.State != w.state || mf.SizeBytes != w.size {
			t.Errorf("%s = %s/%d, want %s/%d", path, mf.State, mf.SizeBytes, w.state, w.size)
		}
	}
	if mf, _ := st.ModelFileByID(rows["Qwen/deleted"].ID); mf.Message != MessageMissingInStorage {
		t.Errorf("missing message = %q", mf.Message)
	}
	if mf, _ := st.ModelFileByID(other.ID); mf.State != proto.MFPresent {
		t.Errorf("a row in another storage changed: %+v", mf)
	}
	// A missing file no longer serves launches from the storage.
	loc, err := st.ModelLocation(rows["Qwen/deleted"].ModelID, f.nfs.ID, f.n1.ID)
	if err != nil || loc.Ready || loc.Reason != LocationModelMissingInStore {
		t.Fatalf("location of a missing file = %+v err=%v", loc, err)
	}
	// Applying the same page again changes nothing.
	again, err := st.ApplyStorageScan(f.nfs.ID, ScanSighting{
		Covers: func(rel string) bool { return rel != "Z/later-page" },
		Found:  map[string]int64{"Qwen/back": 10, "Qwen/grown": 25},
		Unsure: map[string]bool{"Qwen/unreadable": true, "deep/a/b": true},
		Gone:   func(rel string) bool { return rel == "Qwen/deleted" },
	})
	if err != nil || again != (ScanChanges{}) {
		t.Fatalf("second apply = %+v err=%v", again, err)
	}
}
