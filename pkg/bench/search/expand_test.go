package search

import "testing"

func TestExpandJSONGrid(t *testing.T) {
	got, err := ExpandJSON("grid", []byte(`{"concurrency":[1,2],"isl":[8],"osl":[4,8]}`))
	if err != nil || len(got) != 4 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
func TestExpandJSONScalarConcurrency(t *testing.T) {
	got, err := ExpandJSON("grid", []byte(`{"concurrency":3,"isl": [9],"osl":[5]}`))
	if err != nil || len(got) != 1 || got[0].Concurrency != 3 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
