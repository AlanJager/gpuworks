package proto

import "testing"

func TestScanCoversFollowsPageBounds(t *testing.T) {
	first := StorageScanResult{NextCursor: "Qwen/b"}
	last := StorageScanResult{Complete: true}
	for _, c := range []struct {
		cursor string
		page   StorageScanResult
		rel    string
		want   bool
	}{
		{"", first, "Qwen", true},                        // shallower paths come first
		{"", first, "Qwen/a", true},                      //
		{"", first, "Qwen/b", true},                      // the cursor itself was reported on this page
		{"", first, "Qwen/c", false},                     // left for the next page
		{"", first, "A/b/c", false},                      // deeper paths come after every depth-2 path
		{"Qwen/b", last, "Qwen/b", false},                // reported by the previous page
		{"Qwen/b", last, "Qwen/c", true},                 //
		{"Qwen/b", last, "A/b/c", true},                  //
		{"Qwen/b", StorageScanResult{}, "Qwen/c", false}, // an interrupted page with no progress covers nothing
	} {
		if got := ScanCovers(c.cursor, c.page, c.rel); got != c.want {
			t.Errorf("ScanCovers(%q, %+v, %q) = %v, want %v", c.cursor, c.page, c.rel, got, c.want)
		}
	}
	if ScanSkipUndecided(ScanSkipEmpty) || ScanSkipUndecided(ScanSkipNoWeights) || !ScanSkipUndecided(ScanSkipUnreadable) || !ScanSkipUndecided(ScanSkipTooDeep) {
		t.Fatal("ScanSkipUndecided misclassifies")
	}
}
