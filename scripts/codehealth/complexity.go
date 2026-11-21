package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"sort"
)

// funcStat is what one function costs a reader.
type funcStat struct {
	File, Name string
	Line       int
	// Branches is 1 plus one per if, for, range, non-default case, select
	// arm, && and ||: the paths through the function.
	Branches int
	// ErrChecks are the ifs that compare a value named err with nil. A
	// function high in Branches and nearly as high here is a long sequence
	// of steps, not a tangle of decisions.
	ErrChecks int
	// Nesting is the deepest if/for/range inside another; an else-if chain
	// stays at the depth of its first if.
	Nesting int
	Lines   int
}

func (s funcStat) key() string { return s.File + "\t" + s.Name }

// measure returns the functions of one Go source file. A function literal
// counts toward the function that holds it.
func measure(path string, src []byte) ([]funcStat, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []funcStat
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		s := funcStat{File: path, Name: funcName(fn), Line: fset.Position(fn.Pos()).Line, Branches: 1,
			Lines: fset.Position(fn.End()).Line - fset.Position(fn.Pos()).Line + 1}
		walk(fn.Body, &s, 0)
		out = append(out, s)
	}
	return out, nil
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		case *ast.Ident:
			return x.Name + "." + fn.Name.Name
		}
		return fn.Name.Name
	}
}

func walk(n ast.Node, s *funcStat, depth int) {
	if n == nil {
		return
	}
	deeper := func(body ast.Node) {
		if depth+1 > s.Nesting {
			s.Nesting = depth + 1
		}
		walk(body, s, depth+1)
	}
	ast.Inspect(n, func(c ast.Node) bool {
		if c == nil || c == n {
			return true
		}
		switch x := c.(type) {
		case *ast.IfStmt:
			walkIf(x, s, depth)
			return false
		case *ast.ForStmt:
			s.Branches++
			walk(x.Init, s, depth)
			walk(x.Cond, s, depth)
			walk(x.Post, s, depth)
			deeper(x.Body)
			return false
		case *ast.RangeStmt:
			s.Branches++
			walk(x.X, s, depth)
			deeper(x.Body)
			return false
		case *ast.CaseClause:
			if x.List != nil {
				s.Branches++
			}
		case *ast.CommClause:
			if x.Comm != nil {
				s.Branches++
			}
		case *ast.BinaryExpr:
			if x.Op == token.LAND || x.Op == token.LOR {
				s.Branches++
			}
		}
		return true
	})
}

func walkIf(x *ast.IfStmt, s *funcStat, depth int) {
	s.Branches++
	if isErrCheck(x.Cond) {
		s.ErrChecks++
	}
	if depth+1 > s.Nesting {
		s.Nesting = depth + 1
	}
	walk(x.Init, s, depth)
	// The condition is walked through a wrapper so that a top-level && or
	// || is seen as a child, not skipped as the root.
	walk(&ast.ParenExpr{X: x.Cond}, s, depth)
	walk(x.Body, s, depth+1)
	switch e := x.Else.(type) {
	case *ast.IfStmt:
		walkIf(e, s, depth)
	case *ast.BlockStmt:
		walk(e, s, depth+1)
	}
}

func isErrCheck(cond ast.Expr) bool {
	b, ok := cond.(*ast.BinaryExpr)
	if !ok || (b.Op != token.NEQ && b.Op != token.EQL) {
		return false
	}
	x, xok := b.X.(*ast.Ident)
	y, yok := b.Y.(*ast.Ident)
	return xok && yok && ((isErrName(x.Name) && y.Name == "nil") || (isErrName(y.Name) && x.Name == "nil"))
}

func isErrName(name string) bool {
	return name == "err" || (len(name) > 3 && (name[len(name)-3:] == "Err" || name[:3] == "err"))
}

func measureCommit(commit string) (map[string]funcStat, error) {
	stats := map[string]funcStat{}
	err := eachGoFile(commit, func(path string, src []byte) {
		funcs, err := measure(path, src)
		if err != nil {
			return // a file that does not parse is the build's to report
		}
		for _, f := range funcs {
			stats[f.key()] = f
		}
	})
	return stats, err
}

// thresholds are the levels the distribution is counted at.
var thresholds = []int{15, 25, 40}

func reportComplexity(w io.Writer, base, head string, top int) error {
	before, err := measureCommit(base)
	if err != nil {
		return err
	}
	after, err := measureCommit(head)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "## Complexity\n\nBranches = 1 + if/for/range/case/select arm/&&/||. \"err\" is how many of the ifs only check an error.\n\n")
	fmt.Fprintf(w, "| | base | head |\n|---|---|---|\n| functions | %d | %d |\n", len(before), len(after))
	count := func(m map[string]funcStat, ok func(funcStat) bool) (n int) {
		for _, f := range m {
			if ok(f) {
				n++
			}
		}
		return n
	}
	for _, t := range thresholds {
		at := func(f funcStat) bool { return f.Branches >= t }
		fmt.Fprintf(w, "| branches ≥ %d | %d | %d |\n", t, count(before, at), count(after, at))
	}
	deep := func(f funcStat) bool { return f.Nesting >= 5 }
	long := func(f funcStat) bool { return f.Lines >= 150 }
	fmt.Fprintf(w, "| nesting ≥ 5 | %d | %d |\n| lines ≥ 150 | %d | %d |\n\n", count(before, deep), count(after, deep), count(before, long), count(after, long))

	all := make([]funcStat, 0, len(after))
	for _, f := range after {
		all = append(all, f)
	}
	byBranches := func(a, b funcStat) bool {
		if a.Branches != b.Branches {
			return a.Branches > b.Branches
		}
		return a.key() < b.key()
	}
	sort.Slice(all, func(i, j int) bool { return byBranches(all[i], all[j]) })
	was := func(f funcStat) string {
		if b, ok := before[f.key()]; ok {
			return fmt.Sprint(b.Branches)
		}
		return "new"
	}
	row := func(f funcStat) {
		fmt.Fprintf(w, "| %d | %s | %d | %d | %d | `%s:%d` %s |\n", f.Branches, was(f), f.ErrChecks, f.Nesting, f.Lines, f.File, f.Line, f.Name)
	}
	header := "| branches | at base | err | nesting | lines | function |\n|---|---|---|---|---|---|\n"

	fmt.Fprintf(w, "### Highest at head\n\n%s", header)
	for i, f := range all {
		if i == top {
			break
		}
		row(f)
	}

	grown := all[:0:0]
	for _, f := range all {
		if b, ok := before[f.key()]; ok && f.Branches-b.Branches >= 3 {
			grown = append(grown, f)
		}
	}
	sort.SliceStable(grown, func(i, j int) bool {
		return grown[i].Branches-before[grown[i].key()].Branches > grown[j].Branches-before[grown[j].key()].Branches
	})
	fmt.Fprintf(w, "\n### Grew most since base (%d functions grew by 3 or more)\n\n%s", len(grown), header)
	for i, f := range grown {
		if i == top {
			break
		}
		row(f)
	}

	fmt.Fprintf(w, "\n### Deepest nesting at head\n\n%s", header)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Nesting > all[j].Nesting })
	for i, f := range all {
		if i == top/2 || f.Nesting < 5 {
			break
		}
		row(f)
	}

	newByDir := map[string]int{}
	for _, f := range after {
		if _, ok := before[f.key()]; !ok && f.Branches >= 20 {
			newByDir[dirOf(f.File)]++
		}
	}
	fmt.Fprintf(w, "\n### New functions with branches ≥ 20, by package\n\n| count | package |\n|---|---|\n")
	for i, kv := range sortedCounts(newByDir) {
		if i == top/2 {
			break
		}
		fmt.Fprintf(w, "| %d | `%s` |\n", kv.n, kv.k)
	}
	fmt.Fprintln(w)
	return nil
}
