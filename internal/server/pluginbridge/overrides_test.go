package pluginbridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

var alice = Administrator{UserID: 7, Username: "alice"}

// pushHolds pushes node holds as installed package id, the way its host
// connection would.
func pushHolds(t *testing.T, b *Bridge, w *world, id string, gen uint64, body string) {
	t.Helper()
	pusher := pushstate.Pusher{ID: id, Publisher: "acme", Grants: w.pub.Cert.Grants, Permissions: []string{"push:node_holds"},
		Codes: []string{"ACME_MAINTENANCE", "ACME_QUARANTINE"}}
	if _, werr := b.State().Push(context.Background(), pusher, &extapi.StatePush{Kind: extapi.KindNodeHolds, Schema: 1, Generation: gen, Body: []byte(body)}); werr != nil {
		t.Fatalf("push = %s %v", werr.Code, werr.Params)
	}
}

func holdOf(id uint) string {
	return fmt.Sprintf(`{"nodes":{"%d":{"code":"ACME_MAINTENANCE"}}}`, id)
}

// auditLines are the audit lines the bridges logged for action.
func auditLines(w *world, action string) []string {
	var out []string
	for _, line := range strings.Split(w.logs.String(), "\n") {
		if strings.Contains(line, "audit=true") && strings.Contains(line, "action="+action+" ") {
			out = append(out, line)
		}
	}
	return out
}

// An override needs a reason, releases the hold, is listed, and is
// withdrawn; each call, refused or not, leaves an audit line naming the
// administrator by id and by name.
func TestOverridesAreAuditedAndNeedAReason(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	pushHolds(t, b, w, "acme_backup", 1, holdOf(12))
	req := OverrideRequest{Owner: "acme_backup", Kind: extapi.KindNodeHolds, Entry: "12", Action: pushstate.OverrideDelete, By: alice}

	for _, reason := range []string{"", "   ", strings.Repeat("x", MaxOverrideReason+1)} {
		req.Reason = reason
		if _, err := b.Override(context.Background(), req); !isCode(err, messages.ErrorPluginOverrideRejected) {
			t.Fatalf("override with reason %q = %v", reason, err)
		}
	}
	if lines := auditLines(w, "override"); len(lines) != 3 || !strings.Contains(lines[0], "outcome=refused code=PLUGIN_OVERRIDE_REJECTED") {
		t.Fatalf("audit of refused overrides = %q", lines)
	}
	req.Entry = "13"
	req.Reason = "maintenance done"
	if _, err := b.Override(context.Background(), req); !isCode(err, messages.ErrorPluginOverrideRejected) {
		t.Fatalf("override of a node the plugin does not hold = %v", err)
	}
	req.Entry = "12"
	o, err := b.Override(context.Background(), req)
	if err != nil || o.ID == 0 || o.ByUser != 7 || o.Reason != "maintenance done" {
		t.Fatalf("override = %+v, %v", o, err)
	}
	if b.State().Hold(12, 8).Code != "" {
		t.Fatal("the override did not release the hold")
	}
	if list := b.Overrides(); len(list) != 1 || list[0].ID != o.ID {
		t.Fatalf("overrides = %+v", list)
	}
	lines := auditLines(w, "override")
	ok := lines[len(lines)-1]
	for _, want := range []string{"plugin=acme_backup", "kind=node_holds", "entry=12", fmt.Sprintf("override=%d", o.ID), "by_user=7", "by=alice", "outcome=ok", `reason="maintenance done"`} {
		if !strings.Contains(ok, want) {
			t.Fatalf("audit line %q lacks %s", ok, want)
		}
	}

	if _, err := b.RemoveOverride(context.Background(), o.ID, alice); err != nil {
		t.Fatal(err)
	}
	if b.State().Hold(12, 8).Code != "ACME_MAINTENANCE" || len(b.Overrides()) != 0 {
		t.Fatal("the removal did not enforce the hold again")
	}
	if _, err := b.RemoveOverride(context.Background(), o.ID, alice); !isCode(err, messages.ErrorPluginOverrideNotFound) {
		t.Fatalf("second removal = %v", err)
	}
	removed := auditLines(w, "remove_override")
	if len(removed) != 2 || !strings.Contains(removed[0], "plugin=acme_backup") || !strings.Contains(removed[0], "outcome=ok") ||
		!strings.Contains(removed[1], "outcome=refused code=PLUGIN_OVERRIDE_NOT_FOUND") || !strings.Contains(removed[1], "by=alice") {
		t.Fatalf("audit of removals = %q", removed)
	}
}

// A push that changes the overridden entry supersedes the override: it is
// audited and the server's own effect still runs.
func TestASupersededOverrideIsAudited(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	pushHolds(t, b, w, "acme_backup", 1, holdOf(12))
	o, err := b.Override(context.Background(), OverrideRequest{Owner: "acme_backup", Kind: extapi.KindNodeHolds, Entry: "12",
		Action: pushstate.OverrideDelete, Reason: "relief", By: alice})
	if err != nil {
		t.Fatal(err)
	}
	pushHolds(t, b, w, "acme_backup", 2, `{"nodes":{"12":{"code":"ACME_QUARANTINE"}}}`)
	lines := auditLines(w, "override_superseded")
	if len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf("override=%d", o.ID)) || !strings.Contains(lines[0], "plugin=acme_backup") {
		t.Fatalf("supersede audit = %q", lines)
	}
	if n := w.announced.Load(); n == 0 {
		t.Fatal("the server's own effects stopped running")
	}
}

// A package that no longer verifies keeps what it restricted, and an
// administrator can still release it.
func TestABrokenPackagesHoldCanBeOverridden(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	pushHolds(t, b, w, "acme_backup", 1, holdOf(12))
	bin := filepath.Join(b.pkgs.Dir.Version("acme_backup", "1.0.0"), "bin", "linux_amd64", "acme_backup")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	b = w.open()
	if v := b.List()[0]; v.State != StateBroken {
		t.Fatalf("tampered package = %+v", v)
	}
	if b.State().Hold(12, 8).Code != "ACME_MAINTENANCE" {
		t.Fatal("a broken package's hold is not enforced")
	}
	if _, err := b.Override(context.Background(), OverrideRequest{Owner: "acme_backup", Kind: extapi.KindNodeHolds, Entry: "12",
		Action: pushstate.OverrideDelete, Reason: "the package is broken", By: alice}); err != nil {
		t.Fatal(err)
	}
	if b.State().Hold(12, 8).Code != "" {
		t.Fatal("the override of a broken package's hold did not release it")
	}
}
