// Command structcount measures how much state and behaviour one struct type
// carries, for the architecture ratchet (scripts/check_architecture.sh,
// ADR 0066). It parses the package's non-test files and prints one number:
//
//	-measure fields   field names, counting inside anonymous structs (wrapping
//	                  fields in one does not make them leave the type)
//	-measure mutexes  sync.Mutex / sync.RWMutex fields, by value or pointer,
//	                  inside anonymous structs too
//	-measure maps     map fields, sync.Map fields and fields of a map type
//	                  declared in the package, inside anonymous structs too
//	-measure methods  functions with the type as receiver (value or pointer)
//	                  or as a parameter, whatever their own receiver
//	-measure rpcs     distinct proto.RPC* constants the package dispatches on:
//	                  switch case values (a helper switch counts too),
//	                  operands of == and !=, and keys of a map literal whose
//	                  value type names the type or that sits in a function
//	                  taking the type. A method of another type that does
//	                  not take the type is not looked into
//
// A field of a named struct type counts once: moving state into a type of
// its own is how it leaves the composition root.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	dir := flag.String("dir", "", "package directory")
	typ := flag.String("type", "", "struct type name")
	measure := flag.String("measure", "", "fields | mutexes | maps | methods | rpcs")
	flag.Parse()
	n, err := count(*dir, *typ, *measure)
	if err != nil {
		fmt.Fprintln(os.Stderr, "structcount:", err)
		os.Exit(1)
	}
	fmt.Println(n)
}

func count(dir, typ, measure string) (int, error) {
	files, err := parsePackage(dir)
	if err != nil {
		return 0, err
	}
	switch measure {
	case "methods":
		return countMethods(files, typ), nil
	case "rpcs":
		return countRPCCases(files, typ), nil
	case "fields", "mutexes", "maps":
	default:
		return 0, fmt.Errorf("unknown measure %q", measure)
	}
	types := localTypes(files)
	spec, ok := types[typ]
	if !ok {
		return 0, fmt.Errorf("type %s not found in %s", typ, dir)
	}
	st, ok := spec.Type.(*ast.StructType)
	if !ok {
		return 0, fmt.Errorf("%s is not a struct", typ)
	}
	return countFields(st, measure, types), nil
}

func parsePackage(dir string) ([]*ast.File, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go files in %s", dir)
	}
	return files, nil
}

func localTypes(files []*ast.File) map[string]*ast.TypeSpec {
	out := map[string]*ast.TypeSpec{}
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.TYPE {
				continue
			}
			for _, s := range g.Specs {
				ts := s.(*ast.TypeSpec)
				out[ts.Name.Name] = ts
			}
		}
	}
	return out
}

func countFields(st *ast.StructType, measure string, types map[string]*ast.TypeSpec) int {
	n := 0
	for _, f := range st.Fields.List {
		names := len(f.Names)
		if names == 0 {
			names = 1 // embedded
		}
		t := f.Type
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		if inner, ok := t.(*ast.StructType); ok {
			n += names * countFields(inner, measure, types)
			if measure == "fields" {
				n += names
			}
			continue
		}
		switch measure {
		case "fields":
			n += names
		case "mutexes":
			if isSync(t, "Mutex") || isSync(t, "RWMutex") {
				n += names
			}
		case "maps":
			if isMap(t, types) {
				n += names
			}
		}
	}
	return n
}

func isSync(t ast.Expr, name string) bool {
	sel, ok := t.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "sync" && sel.Sel.Name == name
}

func isMap(t ast.Expr, types map[string]*ast.TypeSpec) bool {
	switch t := t.(type) {
	case *ast.MapType:
		return true
	case *ast.SelectorExpr:
		return isSync(t, "Map")
	case *ast.Ident:
		if spec, ok := types[t.Name]; ok {
			_, isMapType := spec.Type.(*ast.MapType)
			return isMapType
		}
	}
	return false
}

func countMethods(files []*ast.File, typ string) int {
	n := 0
	for _, f := range files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && takes(fn, typ) {
				n++
			}
		}
	}
	return n
}

// takes reports whether fn has typ as its receiver or as a parameter.
func takes(fn *ast.FuncDecl, typ string) bool {
	for _, list := range []*ast.FieldList{fn.Recv, fn.Type.Params} {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			if mentions(field.Type, typ) {
				return true
			}
		}
	}
	return false
}

// mentions reports whether a type expression names typ.
func mentions(expr ast.Expr, typ string) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && id.Name == typ {
			found = true
		}
		return !found
	})
	return found
}

func countRPCCases(files []*ast.File, typ string) int {
	seen := map[string]bool{}
	see := func(e ast.Expr) {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "proto" && strings.HasPrefix(sel.Sel.Name, "RPC") {
			seen[sel.Sel.Name] = true
		}
	}
	// inspect counts the RPCs dispatched under root. anyMap is set inside a
	// function that takes typ, where a table of closures reaches it too.
	inspect := func(root ast.Node, anyMap bool) {
		ast.Inspect(root, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CaseClause:
				for _, e := range n.List {
					see(e)
				}
			case *ast.BinaryExpr:
				if n.Op == token.EQL || n.Op == token.NEQ {
					see(n.X)
					see(n.Y)
				}
			case *ast.CompositeLit:
				if m, ok := n.Type.(*ast.MapType); ok && (anyMap || mentions(m.Value, typ)) {
					for _, e := range n.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							see(kv.Key)
						}
					}
				}
			}
			return true
		})
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			switch {
			case !ok:
				inspect(d, false)
			case takes(fn, typ):
				inspect(fn, true)
			case fn.Recv == nil:
				inspect(fn, false)
			}
			// A method of another type that does not take typ dispatches
			// that type's own RPCs.
		}
	}
	return len(seen)
}
