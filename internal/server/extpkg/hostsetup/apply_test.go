package hostsetup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// rootRunner stands in for sudo: it carries out the file commands host
// setup issues (mkdir, tee, chmod, rm) on the test's directories exactly as
// the coreutils would, records every command, refuses any path outside the
// test root and any command it does not know, and fails the command named
// by failOn.
type rootRunner struct {
	t      *testing.T
	root   string
	cmds   []string
	failOn string
	// groups are the groups and their members, as groupadd and usermod
	// leave them.
	groups map[string][]string
}

func (r *rootRunner) Run(_ context.Context, c Cmd) error {
	line := strings.TrimSpace(c.Path + " " + strings.Join(c.Args, " "))
	r.cmds = append(r.cmds, line)
	if r.failOn != "" && strings.HasPrefix(line, r.failOn) {
		return errors.New("exit status 1")
	}
	args := slices.DeleteFunc(slices.Clone(c.Args), func(a string) bool { return a == "--" })
	inRoot := func(p string) string {
		if !strings.HasPrefix(p, r.root+string(filepath.Separator)) {
			r.t.Fatalf("%s touches %s outside the test root", line, p)
		}
		return p
	}
	switch c.Path {
	case "mkdir":
		if len(args) != 4 || args[0] != "-p" || args[1] != "-m" {
			r.t.Fatalf("unexpected %s", line)
		}
		return os.MkdirAll(inRoot(args[3]), 0o755)
	case "tee":
		return os.WriteFile(inRoot(args[0]), c.Stdin, 0o600)
	case "chmod":
		if args[0] != "0644" {
			r.t.Fatalf("unexpected mode in %s", line)
		}
		return os.Chmod(inRoot(args[1]), 0o644)
	case "rm":
		if err := os.Remove(inRoot(args[1])); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	case "udevadm":
		return nil
	case "groupadd":
		r.groups[args[len(args)-1]] = nil
		return nil
	case "usermod":
		g := args[2]
		if _, ok := r.groups[g]; !ok {
			return errors.New("usermod: group does not exist")
		}
		r.groups[g] = append(r.groups[g], args[3])
		return nil
	}
	r.t.Fatalf("unknown command %s", line)
	return nil
}

func (r *rootRunner) take() []string {
	out := r.cmds
	r.cmds = nil
	return out
}

func newHost(t *testing.T) (Host, *rootRunner) {
	root := t.TempDir()
	rules := filepath.Join(root, "rules.d")
	if err := os.Mkdir(rules, 0o755); err != nil {
		t.Fatal(err)
	}
	run := &rootRunner{t: t, root: root, groups: map[string][]string{}}
	h := Host{RulesDir: rules, Records: Records{Dir: filepath.Join(root, "plugins"), Owner: os.Getuid()}, Run: run,
		GroupExists: func(g string) (bool, error) { _, ok := run.groups[g]; return ok, nil },
		InGroup: func(u, g string) (bool, error) {
			return slices.Contains(run.groups[g], u), nil
		}}
	return h, run
}

// The first apply creates the group, adds the service account, writes the
// rule file, reloads and re-triggers udev and then writes the record; the
// same apply again issues no command at all.
func TestApplyWritesRulesThenRecordAndIsIdempotent(t *testing.T) {
	h, run := newHost(t)
	want := Want{"acme_key": {key}}
	rep, err := h.Apply(context.Background(), want, "gpuworks", true)
	if err != nil {
		t.Fatal(err)
	}
	cmds := run.take()
	order := []string{"groupadd", "usermod", "tee -- " + h.RulesDir, "udevadm control", "udevadm trigger", "tee -- " + h.Records.Dir}
	at := 0
	for _, c := range cmds {
		if at < len(order) && strings.HasPrefix(c, order[at]) {
			at++
		}
	}
	if at != len(order) {
		t.Fatalf("commands %q are not in the order %q", cmds, order)
	}
	if !rep.GroupCreated || !rep.UserAdded || !rep.Reloaded || !slices.Equal(rep.Written, []string{"acme_key"}) || len(rep.Pruned) != 0 {
		t.Fatalf("report %+v", rep)
	}
	name, _ := RuleFile("acme_key")
	got, _ := os.ReadFile(filepath.Join(h.RulesDir, name))
	want1, _ := Rules("acme_key", []extapi.HostSetupStep{key})
	if !bytes.Equal(got, want1) {
		t.Fatalf("rule file:\n%s", got)
	}
	if ok, err := h.Records.Matches("acme_key", []extapi.HostSetupStep{key}); !ok || err != nil {
		t.Fatalf("record matches = %v, %v", ok, err)
	}
	rep, err = h.Apply(context.Background(), want, "gpuworks", true)
	if err != nil || rep.Changed() {
		t.Fatalf("second apply = %+v, %v", rep, err)
	}
	if cmds := run.take(); len(cmds) != 0 {
		t.Fatalf("second apply ran %q", cmds)
	}
}

// Changed steps rewrite the file and the record; a package no longer
// wanted loses both under prune and keeps them without it.
func TestApplyRewritesAndPrunes(t *testing.T) {
	h, run := newHost(t)
	ctx := context.Background()
	if _, err := h.Apply(ctx, Want{"acme_key": {key}, "acme_old": {second}}, "", true); err != nil {
		t.Fatal(err)
	}
	run.take()
	rep, err := h.Apply(ctx, Want{"acme_key": {key, second}}, "", false)
	if err != nil || !slices.Equal(rep.Written, []string{"acme_key"}) || len(rep.Pruned) != 0 {
		t.Fatalf("apply one without prune = %+v, %v", rep, err)
	}
	if ids, _ := h.Installed(); !slices.Equal(ids, []string{"acme_key", "acme_old"}) {
		t.Fatalf("installed without prune = %v", ids)
	}
	if ok, _ := h.Records.Matches("acme_key", []extapi.HostSetupStep{key}); ok {
		t.Fatal("the record still matches the old steps")
	}
	run.take()
	rep, err = h.Apply(ctx, Want{"acme_key": {key, second}}, "", true)
	if err != nil || !slices.Equal(rep.Pruned, []string{"acme_old"}) || !rep.Reloaded {
		t.Fatalf("prune = %+v, %v", rep, err)
	}
	if ids, _ := h.Installed(); !slices.Equal(ids, []string{"acme_key"}) {
		t.Fatalf("installed after prune = %v", ids)
	}
	rep, err = h.Apply(ctx, nil, "", true)
	if err != nil || !slices.Equal(rep.Pruned, []string{"acme_key"}) {
		t.Fatalf("prune everything = %+v, %v", rep, err)
	}
	if ids, _ := h.Installed(); len(ids) != 0 {
		t.Fatalf("installed after pruning everything = %v", ids)
	}
	for _, c := range run.take() {
		if strings.HasPrefix(c, "groupadd") || strings.HasPrefix(c, "usermod") {
			t.Fatalf("pruning changed the user database: %s", c)
		}
	}
}

// A failed udev reload leaves no record, so the package stays pending and
// the next apply tries again.
func TestApplyWritesNoRecordWhenUdevFails(t *testing.T) {
	h, run := newHost(t)
	run.failOn = "udevadm trigger"
	if _, err := h.Apply(context.Background(), Want{"acme_key": {key}}, "", true); err == nil {
		t.Fatal("apply succeeded with udev failing")
	}
	if _, err := h.Records.Read("acme_key"); !errors.Is(err, ErrNotApplied) {
		t.Fatalf("record after a failed trigger: %v", err)
	}
	run.failOn = ""
	run.take()
	rep, err := h.Apply(context.Background(), Want{"acme_key": {key}}, "", true)
	if err != nil || !rep.Reloaded || !slices.Equal(rep.Written, []string{"acme_key"}) {
		t.Fatalf("retry = %+v, %v", rep, err)
	}
	if ok, _ := h.Records.Matches("acme_key", []extapi.HostSetupStep{key}); !ok {
		t.Fatal("no record after the retry")
	}
}

// A record counts only when its owner wrote it alone: one writable by the
// group, one owned by someone else or one without a digest does not match.
func TestRecordsRefuseWhatTheOwnerDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	r := Records{Dir: dir, Owner: os.Getuid()}
	d, _ := Digest([]extapi.HostSetupStep{key})
	path, _ := r.Path("acme_key")
	if ok, err := r.Matches("acme_key", []extapi.HostSetupStep{key}); ok || err != nil {
		t.Fatalf("no record = %v, %v", ok, err)
	}
	if ok, err := r.Matches("acme_key", nil); !ok || err != nil {
		t.Fatalf("no steps = %v, %v", ok, err)
	}
	if err := os.WriteFile(path, Content(d), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read("acme_key"); err == nil {
		t.Fatal("a group-writable record was read")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Read("acme_key"); err != nil || got != d {
		t.Fatalf("record = %s, %v", got, err)
	}
	other := Records{Dir: dir, Owner: os.Getuid() + 1}
	if ok, err := other.Matches("acme_key", []extapi.HostSetupStep{key}); ok || err == nil {
		t.Fatalf("a record of another owner = %v, %v", ok, err)
	}
	if err := os.WriteFile(path, []byte("applied\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read("acme_key"); err == nil {
		t.Fatal("a record without a digest was read")
	}
}
