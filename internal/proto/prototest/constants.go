// Package prototest lets a test enumerate package proto's constants, so a
// table that must cover every RPC or stream fails when one is added.
package prototest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Constants is the string constants declared in the proto package at dir
// whose names start with prefix, by name.
func Constants(t testing.TB, dir, prefix string) map[string]string {
	t.Helper()
	pkgs, err := parser.ParseDir(token.NewFileSet(), dir, func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, prefix) || i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						value, _ := strconv.Unquote(lit.Value)
						out[name.Name] = value
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s constants found in %s", prefix, dir)
	}
	return out
}
