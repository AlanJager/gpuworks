package gpuallocation_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This guards the catalog's traceability, not behavior or completeness: each
// rule must still be reviewed against the actual assertions of its named cases.
func TestCoverageCatalogReferences(t *testing.T) {
	raw, err := os.ReadFile("COVERAGE.md")
	if err != nil {
		t.Fatal(err)
	}
	rulePattern := regexp.MustCompile(`^\| ([A-Z]+-[0-9]+) \|`)
	linkPattern := regexp.MustCompile(`\[([^\]]+)\]\(([^)]+\.go)\)`)
	seen := map[string]bool{}
	parsed := map[string]map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		rule := rulePattern.FindStringSubmatch(line)
		if rule == nil {
			continue
		}
		if seen[rule[1]] {
			t.Errorf("duplicate rule %s", rule[1])
		}
		seen[rule[1]] = true
		t.Run(rule[1], func(t *testing.T) {
			tests, owners := 0, 0
			for _, link := range linkPattern.FindAllStringSubmatch(line, -1) {
				label, path := link[1], filepath.Clean(link[2])
				if _, err := os.Stat(path); err != nil {
					t.Errorf("missing source %s: %v", path, err)
					continue
				}
				if !strings.HasSuffix(path, "_test.go") {
					owners++
					continue
				}
				tests++
				functions, ok := parsed[path]
				if !ok {
					file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
					if err != nil {
						t.Fatal(err)
					}
					functions = map[string]bool{}
					for _, decl := range file.Decls {
						fn, ok := decl.(*ast.FuncDecl)
						if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
							functions[fn.Name.Name] = true
						}
					}
					parsed[path] = functions
				}
				if !functions[label] {
					t.Errorf("%s has no test %s", path, label)
				}
			}
			if tests == 0 || owners == 0 {
				t.Error("rule needs an implementation owner and a named executable test")
			}
		})
	}
	if len(seen) == 0 {
		t.Fatal("no allocation rules found")
	}
}
