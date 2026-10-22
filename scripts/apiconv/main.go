// Command apiconv checks the HTTP API's response conventions
// (docs/reference/api-conventions.md) where they can be read off the types:
//
//   - a collection is answered as an object whose items field holds the
//     elements, never as a bare JSON array;
//   - every JSON field name is snake_case.
//
// It type-checks internal/server/api and inspects the value passed to each
// response writer (writeJSON, writeSSE). Field names are read from the json
// tags of every struct reachable from that value, and from the string keys of
// map literals written in place. Values typed any are opaque to it; a type with
// its own MarshalJSON is trusted to follow the convention.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

const apiPkg = "gpuworks.ai/gpuworks/internal/server/api"

// sinks are the response writers and the index of the argument they encode.
var sinks = map[string]int{"writeJSON": 2, "writeSSE": 2}

var snakeCase = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

type violation struct {
	pos  token.Position
	rule string
	what string
}

func main() {
	dir := flag.String("C", ".", "module root")
	opaque := flag.Bool("opaque", false, "print the number of opaque response fields (any, map[string]any, json.RawMessage) and exit")
	verbose := flag.Bool("v", false, "with -opaque, list the fields")
	flag.Parse()
	found, opaqueFields, err := check(*dir, apiPkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apiconv:", err)
		os.Exit(2)
	}
	if *opaque {
		if *verbose {
			names := make([]string, 0, len(opaqueFields))
			for name := range opaqueFields {
				names = append(names, name)
			}
			sort.Strings(names)
			fmt.Println(strings.Join(names, "\n"))
		}
		fmt.Println(len(opaqueFields))
		return
	}
	for _, v := range found {
		fmt.Printf("%s: %s: %s\n", v.pos, v.rule, v.what)
	}
	if len(found) > 0 {
		fmt.Printf("apiconv: %d violation(s) of docs/reference/api-conventions.md\n", len(found))
		os.Exit(1)
	}
	fmt.Println("apiconv: ok")
}

func check(dir, pattern string) ([]violation, map[string]bool, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  dir,
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, nil, err
	}
	if packages.PrintErrors(pkgs) > 0 {
		return nil, nil, fmt.Errorf("%s does not type-check", pattern)
	}
	var out []violation
	seen := map[string]bool{}
	opaque := map[string]bool{}
	for _, pkg := range pkgs {
		c := &checker{fset: pkg.Fset, info: pkg.TypesInfo, seen: seen, opaque: opaque}
		for _, file := range pkg.Syntax {
			if strings.HasSuffix(pkg.Fset.Position(file.Pos()).Filename, "_test.go") {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				argIdx, ok := sinks[id.Name]
				if !ok || len(call.Args) <= argIdx {
					return true
				}
				if fn, ok := pkg.TypesInfo.Uses[id].(*types.Func); !ok || fn.Pkg() == nil || fn.Pkg().Path() != pkg.PkgPath {
					return true
				}
				c.sink(id.Name, call.Args[argIdx])
				return true
			})
		}
		out = append(out, c.found...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].pos.Filename != out[j].pos.Filename {
			return out[i].pos.Filename < out[j].pos.Filename
		}
		return out[i].pos.Line < out[j].pos.Line
	})
	return out, opaque, nil
}

type checker struct {
	fset  *token.FileSet
	info  *types.Info
	seen  map[string]bool
	found []violation
	// opaque collects the response fields whose content the types do not
	// show, as "Type.Field".
	opaque map[string]bool
}

func (c *checker) report(pos token.Pos, rule, what string) {
	p := c.fset.Position(pos)
	key := rule + "|" + what
	if rule == "envelope" {
		key += "|" + p.String()
	}
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.found = append(c.found, violation{pos: p, rule: rule, what: what})
}

func (c *checker) sink(writer string, arg ast.Expr) {
	t := c.info.TypeOf(arg)
	if t == nil {
		return
	}
	if writer == "writeJSON" && isCollection(t) {
		c.report(arg.Pos(), "envelope", fmt.Sprintf("response is a bare %s; answer {\"items\": [...]} (dto.List)", types.TypeString(t, shortQualifier)))
	}
	c.expr(arg)
	c.walk(arg.Pos(), t, map[types.Type]bool{})
}

// expr checks map literals written in place: their keys are field names.
func (c *checker) expr(e ast.Expr) {
	switch x := ast.Unparen(e).(type) {
	case *ast.UnaryExpr:
		c.expr(x.X)
	case *ast.CompositeLit:
		if _, ok := c.info.TypeOf(x).Underlying().(*types.Map); ok {
			for _, el := range x.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if key, err := strconv.Unquote(lit.Value); err == nil && !snakeCase.MatchString(key) {
						c.report(lit.Pos(), "snake_case", fmt.Sprintf("map key %q", key))
					}
				}
				c.expr(kv.Value)
				if t := c.info.TypeOf(kv.Value); t != nil {
					c.walk(kv.Value.Pos(), t, map[types.Type]bool{})
				}
			}
			return
		}
		for _, el := range x.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				c.expr(kv.Value)
			} else {
				c.expr(el)
			}
		}
	}
}

// walk checks the json field names of every struct reachable from t.
func (c *checker) walk(pos token.Pos, t types.Type, visiting map[types.Type]bool) {
	if visiting[t] {
		return
	}
	visiting[t] = true
	defer delete(visiting, t)
	if marshalsItself(t) {
		return
	}
	switch u := t.(type) {
	case *types.Pointer:
		c.walk(pos, u.Elem(), visiting)
		return
	case *types.Alias:
		c.walk(pos, types.Unalias(u), visiting)
		return
	}
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		c.walk(pos, u.Elem(), visiting)
	case *types.Slice:
		c.walk(pos, u.Elem(), visiting)
	case *types.Array:
		c.walk(pos, u.Elem(), visiting)
	case *types.Map:
		c.walk(pos, u.Elem(), visiting)
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			f := u.Field(i)
			if !f.Exported() {
				continue
			}
			name, tagged, ok := jsonName(u.Tag(i), f.Name())
			if !ok {
				continue
			}
			if f.Embedded() && !tagged {
				c.walk(pos, f.Type(), visiting)
				continue
			}
			if !snakeCase.MatchString(name) {
				c.report(f.Pos(), "snake_case", fmt.Sprintf("field %s.%s encodes as %q", typeName(t), f.Name(), name))
			}
			if isOpaque(f.Type()) {
				c.opaque[typeName(t)+"."+f.Name()] = true
			}
			c.walk(pos, f.Type(), visiting)
		}
	}
}

// jsonName is the name encoding/json gives a field, whether a json tag named
// it (an untagged embedded struct has its fields promoted instead), and
// ok=false when the field is skipped.
func jsonName(tag, field string) (name string, tagged, ok bool) {
	v, _ := reflect.StructTag(tag).Lookup("json")
	name, _, _ = strings.Cut(v, ",")
	if v == "-" {
		return "", false, false
	}
	if name != "" {
		return name, true, true
	}
	return field, false, true
}

// isOpaque reports whether a field's JSON content cannot be read off its type:
// any, a map or slice of any, or json.RawMessage.
func isOpaque(t types.Type) bool {
	if n, ok := types.Unalias(t).(*types.Named); ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "encoding/json" && n.Obj().Name() == "RawMessage" {
		return true
	}
	switch u := types.Unalias(t).Underlying().(type) {
	case *types.Interface:
		return true
	case *types.Map:
		return isOpaque(u.Elem())
	case *types.Slice:
		return !isBytes(u) && isOpaque(u.Elem())
	case *types.Pointer:
		return isOpaque(u.Elem())
	}
	return false
}

func isCollection(t types.Type) bool {
	switch u := types.Unalias(t).Underlying().(type) {
	case *types.Slice:
		return !isBytes(u)
	case *types.Array:
		return true
	case *types.Pointer:
		return isCollection(u.Elem())
	}
	return false
}

func isBytes(s *types.Slice) bool {
	b, ok := s.Elem().Underlying().(*types.Basic)
	return ok && b.Kind() == types.Byte
}

// marshalsItself reports whether t (or *t) has a MarshalJSON or MarshalText
// method: such types choose their own encoding.
func marshalsItself(t types.Type) bool {
	for _, tt := range []types.Type{t, types.NewPointer(t)} {
		ms := types.NewMethodSet(tt)
		for i := 0; i < ms.Len(); i++ {
			switch ms.At(i).Obj().Name() {
			case "MarshalJSON", "MarshalText":
				return true
			}
		}
	}
	return false
}

func typeName(t types.Type) string { return types.TypeString(t, shortQualifier) }

func shortQualifier(p *types.Package) string { return p.Name() }
