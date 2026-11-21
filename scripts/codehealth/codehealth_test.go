package main

import (
	"reflect"
	"testing"
)

func TestMeasureCountsBranchesNestingAndErrorChecks(t *testing.T) {
	src := `package p

type T[V any] struct{}

func plain() {}

func (t *T[V]) chain(a, b bool, xs []int) error {
	if err := step(); err != nil { // 1 branch, 1 err check
		return err
	}
	if a && b || len(xs) > 3 { // 1 + 2 operators
		for _, x := range xs { // 1, depth 2
			switch {
			case x > 1: // 1
				if x > 2 { // 1, depth 3
				}
			default:
			}
		}
	} else if a { // 1, same depth as its if
	} else {
		go func() {
			select {
			case <-done: // 1
			default:
			}
		}()
	}
	return nil
}
`
	got, err := measure("p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []funcStat{
		{File: "p.go", Name: "plain", Line: 5, Branches: 1, Lines: 1},
		{File: "p.go", Name: "T.chain", Line: 7, Branches: 10, ErrChecks: 1, Nesting: 3, Lines: 24},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("measure:\n got %+v\nwant %+v", got, want)
	}
}

func TestMeasureRejectsSourceThatDoesNotParse(t *testing.T) {
	if _, err := measure("p.go", []byte("package p\nfunc {")); err == nil {
		t.Fatal("no error for broken source")
	}
}

func TestProductionGoLeavesOutTestsGeneratedAndFixtures(t *testing.T) {
	for path, want := range map[string]bool{
		"internal/a/a.go":           true,
		"internal/a/a_test.go":      false,
		"internal/a/docs_gen.go":    false,
		"pkg/x/testdata/in/main.go": false,
		"internal/a/README.md":      false,
	} {
		if got := productionGo(path); got != want {
			t.Errorf("productionGo(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestBudgetsLoosenByKind(t *testing.T) {
	before := parseBudgets("api_fanout\t48\tmax\t# raised: why\nseam_users\t3\tmin\n# comment\n\nui_icons\t9\twarn\nbroken line\n")
	after := parseBudgets("api_fanout\t52\tmax\nseam_users\t2\tmin\nui_icons\t8\twarn\nfresh\t0\tmax\n")
	if len(before) != 3 || before["api_fanout"] != (budget{48, "max"}) {
		t.Fatalf("parsed %+v", before)
	}
	for name, want := range map[string]bool{"api_fanout": true, "seam_users": true, "ui_icons": false} {
		if got := loosened(before[name], after[name]); got != want {
			t.Errorf("loosened(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestParseAddedKeepsFileAndLineOfEachAddedLine(t *testing.T) {
	diff := `diff --git a/x/a.go b/x/a.go
--- a/x/a.go
+++ b/x/a.go
@@ -3,0 +4,2 @@ func f() {
+	// for now
+	g()
@@ -9 +11 @@
-old
+new //nolint:errcheck
`
	got := parseAdded(diff)
	want := []added{{"x/a.go", 4, "\t// for now"}, {"x/a.go", 5, "\tg()"}, {"x/a.go", 11, "new //nolint:errcheck"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAdded:\n got %+v\nwant %+v", got, want)
	}
}

func TestMarkersMatchCommentsNotCode(t *testing.T) {
	cases := []struct {
		marker, line string
		want         bool
	}{
		{"lint suppression", `x := y //nolint:staticcheck // reason`, true},
		{"lint suppression", `// eslint-disable-next-line no-console`, true},
		{"todo marker", `// TODO: split`, true},
		{"todo marker", `todo := newTODO()`, false},
		{"put off in words", `// Every caller may for now; later a layer decides.`, true},
		{"put off in words", `msg := "for now"`, false},
		{"put off in words", `// moving to net/http is a change of its own.`, true},
		{"older-version compatibility", `// legacyPrefix is what an older agent sent.`, true},
		{"older-version compatibility", `legacy := true`, false},
		{"copy kept in step by hand", `// rpcRespBody mirrors the server's rpcResponse.`, true},
	}
	byName := map[string]marker{}
	for _, m := range markers {
		byName[m.name] = m
	}
	for _, c := range cases {
		if got := byName[c.marker].re.MatchString(c.line); got != c.want {
			t.Errorf("%s on %q = %v, want %v", c.marker, c.line, got, c.want)
		}
	}
}
