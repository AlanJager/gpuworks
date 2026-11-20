package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/experiencelaunch"
	"gpuworks.ai/gpuworks/internal/server/pluginexec"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

func TestRunDispatchesAnExtraCommand(t *testing.T) {
	var gotArgs []string
	var gotEnv Env
	var gotCtx context.Context
	opt := Options{Commands: []Command{{
		Name:  "quota",
		Usage: "quota status [--data DIR]",
		Run: func(ctx context.Context, env Env, args []string) error {
			gotCtx, gotEnv, gotArgs = ctx, env, args
			return nil
		},
	}}}
	var stderr bytes.Buffer
	log := discardLog()
	if code := Run(log, []string{"quota", "status", "--data", "/srv"}, &stderr, opt); code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, stderr.String())
	}
	if !slices.Equal(gotArgs, []string{"status", "--data", "/srv"}) {
		t.Fatalf("args = %q, want the arguments after the name", gotArgs)
	}
	if gotCtx == nil || gotEnv.Log != log || gotEnv.Stderr != &stderr || gotEnv.Stdout == nil {
		t.Fatalf("env = %+v, ctx = %v; want the caller's logger and stderr and a stdout", gotEnv, gotCtx)
	}
	if stderr.Len() != 0 {
		t.Fatalf("a dispatched command printed the usage:\n%s", stderr.String())
	}
}

func TestRunReportsAnExtraCommandFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"failure", errors.New("the key is missing"), 1},
		{"help", flag.ErrHelp, 0},
		{"wrapped help", errors.Join(errors.New("parse"), flag.ErrHelp), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logged, nil))
			opt := Options{Commands: []Command{{Name: "quota", Run: func(context.Context, Env, []string) error { return tc.err }}}}
			if code := Run(log, []string{"quota"}, &bytes.Buffer{}, opt); code != tc.code {
				t.Fatalf("exit code = %d, want %d", code, tc.code)
			}
			failed := strings.Contains(logged.String(), `msg="quota failed"`) && strings.Contains(logged.String(), "the key is missing")
			if failed != (tc.code == 1) {
				t.Fatalf("log = %q; the failure must be logged exactly when the exit code is 1", logged.String())
			}
		})
	}
}

func TestUsageListsExtraCommandsAfterTheBuiltins(t *testing.T) {
	opt := Options{Commands: []Command{
		{Name: "quota", Usage: "quota status [--data DIR]", Run: func(context.Context, Env, []string) error { return nil }},
		{Name: "edition-info", Run: func(context.Context, Env, []string) error { return nil }},
	}}
	var stderr bytes.Buffer
	if code := Run(discardLog(), []string{"help"}, &stderr, opt); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	want := "  gpuworks version\n  gpuworks quota status [--data DIR]\n  gpuworks edition-info\n\ninstallation:\n"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("the usage does not list the extra commands after version:\n%s", stderr.String())
	}

	// An unknown name still fails with the usage, extra commands included.
	stderr.Reset()
	if code := Run(discardLog(), []string{"frobnicate"}, &stderr, opt); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "frobnicate"`) || !strings.Contains(stderr.String(), want) {
		t.Fatalf("the rejection or the extra usage is missing:\n%s", stderr.String())
	}
}

// Without extra commands the usage is the community one, with no line added.
func TestUsageWithoutExtraCommandsIsUnchanged(t *testing.T) {
	var stderr bytes.Buffer
	Run(discardLog(), []string{"help"}, &stderr, Options{})
	if !strings.Contains(stderr.String(), "  gpuworks version\n\ninstallation:\n") {
		t.Fatalf("the usage changed without extra commands:\n%s", stderr.String())
	}
}

func TestRunPanicsOnAnAmbiguousExtraCommand(t *testing.T) {
	ok := func(context.Context, Env, []string) error { return nil }
	cases := map[string][]Command{
		"no name":    {{Name: " ", Run: ok}},
		"no run":     {{Name: "quota"}},
		"duplicated": {{Name: "quota", Run: ok}, {Name: "quota", Run: ok}},
	}
	for _, name := range builtinCommands {
		cases["shadows "+name] = []Command{{Name: name, Run: ok}}
	}
	for label, cmds := range cases {
		t.Run(label, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Run did not panic")
				}
			}()
			// The check runs before dispatch, whatever the arguments are.
			Run(discardLog(), nil, &bytes.Buffer{}, Options{Commands: cmds})
		})
	}
}

// builtinCommands lists exactly the names Run's switch answers, so no extra
// command can take one of them and none is refused for no reason. The case
// values are read off the source; the named constants stand for their values.
func TestBuiltinCommandsMatchTheDispatch(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	constants := map[string]string{
		"selfinstall.ConsoleUpgradeProtocolCommand": selfinstall.ConsoleUpgradeProtocolCommand,
		"experiencelaunch.Command":                  experiencelaunch.Command,
		"selfinstall.EditionCommand":                selfinstall.EditionCommand,
		"pluginexec.Command":                        pluginexec.Command,
		"hostIdentityCommand":                       hostIdentityCommand,
		"clusterInstallSupervisorCommand":           clusterInstallSupervisorCommand,
	}
	var dispatched []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Run" {
			continue
		}
		for _, stmt := range fn.Body.List {
			sw, ok := stmt.(*ast.SwitchStmt)
			if !ok {
				continue
			}
			for _, clause := range sw.Body.List {
				for _, expr := range clause.(*ast.CaseClause).List {
					switch v := expr.(type) {
					case *ast.BasicLit:
						name, err := strconv.Unquote(v.Value)
						if err != nil {
							t.Fatal(err)
						}
						dispatched = append(dispatched, name)
					case *ast.Ident:
						name, ok := constants[v.Name]
						if !ok {
							t.Fatalf("case %s: add the constant to this test", v.Name)
						}
						dispatched = append(dispatched, name)
					case *ast.SelectorExpr:
						key := v.X.(*ast.Ident).Name + "." + v.Sel.Name
						name, ok := constants[key]
						if !ok {
							t.Fatalf("case %s: add the constant to this test", key)
						}
						dispatched = append(dispatched, name)
					default:
						t.Fatalf("unexpected case expression %T", expr)
					}
				}
			}
		}
	}
	got, want := slices.Sorted(slices.Values(builtinCommands)), slices.Sorted(slices.Values(dispatched))
	if len(want) == 0 || !slices.Equal(got, want) {
		t.Fatalf("builtinCommands = %q, the switch answers %q", got, want)
	}
}

// The server starts plugins through its own binary; a bad command line is
// a usage failure of the trampoline, not an unknown subcommand.
func TestPluginExecIsDispatched(t *testing.T) {
	if code := Run(discardLog(), []string{pluginexec.Command, "--profile"}, &bytes.Buffer{}, Options{}); code != pluginexec.ExitUsage {
		t.Fatalf("exit %d, want the trampoline's usage code %d", code, pluginexec.ExitUsage)
	}
}
