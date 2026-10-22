package dto

import (
	"encoding/json"
	"testing"
)

// TestNewListEncodesEmptyCollectionAsEmptyArray: an empty collection, nil or
// not, is "items": [] with total 0 - never "items": null.
func TestNewListEncodesEmptyCollectionAsEmptyArray(t *testing.T) {
	for name, items := range map[string][]string{"nil": nil, "empty": {}} {
		raw, err := json.Marshal(NewList(items))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"items":[],"total":0}` {
			t.Fatalf("%s: NewList = %s", name, raw)
		}
	}
}

func TestNewListCountsItems(t *testing.T) {
	type row struct {
		ID int `json:"id"`
	}
	raw, err := json.Marshal(NewList([]row{{ID: 1}, {ID: 2}}))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"items":[{"id":1},{"id":2}],"total":2}` {
		t.Fatalf("NewList = %s", raw)
	}
}

// TestNewCursorPageEncodesEmptyPageAsEmptyArray: a cursor page carries no
// total, and an empty page is "items": [].
func TestNewCursorPageEncodesEmptyPageAsEmptyArray(t *testing.T) {
	for name, items := range map[string][]int{"nil": nil, "empty": {}} {
		raw, err := json.Marshal(NewCursorPage(items))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"items":[]}` {
			t.Fatalf("%s: NewCursorPage = %s", name, raw)
		}
	}
	raw, err := json.Marshal(NewCursorPage([]int{7, 3}))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"items":[7,3]}` {
		t.Fatalf("NewCursorPage = %s", raw)
	}
}
