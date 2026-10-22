package pullref

import (
	"reflect"
	"testing"
)

func TestSameRefComparesRegistryResolvedReferences(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"nginx:1", "docker.io/library/nginx:1", true},
		{"org/app:v1", "docker.io/org/app:v1", true},
		{" ghcr.io/org/app@sha256:aaaa ", "ghcr.io/org/app@sha256:aaaa", true},
		{"ghcr.io/org/app:V1", "ghcr.io/org/app:v1", false},
		{"GHCR.io/Org/app:v1", "ghcr.io/org/app:v1", true},
		{"Nginx", "docker.io/library/nginx", true},
		{"localhost:5000/app:V1", "LOCALHOST:5000/app:V1", true},
		{"localhost:5000/app:V1", "localhost:5000/app:v1", false},
		{"ghcr.io/org/app@sha256:AAAA", "ghcr.io/org/app@sha256:aaaa", true},
		{"ghcr.io/org/app:v1", "ghcr.nju.edu.cn/org/app:v1", false},
		{"", "", false},
		{"  ", "nginx:1", false},
	} {
		if got := SameRef(tc.a, tc.b); got != tc.want {
			t.Errorf("SameRef(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDeclaredFirstPrecedesTheChainAndKeepsFirstSpellings(t *testing.T) {
	declared := []string{" swr.example.com/org/app:v1 ", "", "docker.io/org/app:v1", "swr.example.com/org/app:v1"}
	chain := []string{"quay.io/org/app:v1", "swr.example.com/org/app:v1", "ghcr.io/org/app:V1", "org/app:v1"}
	got := DeclaredFirst(declared, chain)
	want := []string{"swr.example.com/org/app:v1", "docker.io/org/app:v1", "quay.io/org/app:v1", "ghcr.io/org/app:V1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeclaredFirst = %v, want %v", got, want)
	}
	// The inputs are not reordered in place.
	if chain[0] != "quay.io/org/app:v1" || declared[0] != " swr.example.com/org/app:v1 " {
		t.Fatalf("inputs were modified: %v %v", declared, chain)
	}
	if got := DeclaredFirst(nil, []string{"org/app:v1"}); !reflect.DeepEqual(got, []string{"org/app:v1"}) {
		t.Fatalf("no declared sources = %v", got)
	}
	if got := DeclaredFirst(nil, nil); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
}
