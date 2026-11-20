package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// fakeDiff is the approval-required refusal of an upgrade from 1.0.0 to
// 2.0.0, with the diff params the package host sets.
func fakeDiff(id string) error {
	return messages.NewProductError(messages.ErrorPluginApprovalRequired, "upgrading "+id+" needs approval", map[string]any{
		"id": id, "version": "2.0.0", "approve_hash": "sha256:22", "permissions": "http:public,http:routes", "roles": "",
		"from_version": "1.0.0", "permissions_added": "http:public=elevated", "permissions_removed": "push:banners=low",
		"roles_added": "", "roles_removed": "", "capabilities_added": "host.setup@1", "capabilities_removed": "",
		"features_added": "", "features_removed": "", "routes_added": "POST /hooks public", "routes_removed": "",
		"host_setup_added": "", "host_setup_removed": "", "devices_added": "", "devices_removed": "", "ui_mode": "", "isolation": ""})
}

func (f *fakePlugins) Upgrade(_ context.Context, id string, body io.Reader, approve, by string) (dto.PluginUpgraded, error) {
	raw, _ := io.ReadAll(body)
	f.mu.Lock()
	f.body = string(raw)
	f.mu.Unlock()
	if err := f.record("upgrade " + id + " " + approve + " " + by); err != nil {
		return dto.PluginUpgraded{}, err
	}
	if approve != "sha256:22" {
		return dto.PluginUpgraded{}, fakeDiff(id)
	}
	return dto.PluginUpgraded{Plugin: dto.Plugin{ID: id, Version: "2.0.0", State: "running", Parts: []string{}, Roles: []string{},
		Permissions: []dto.PluginPermission{}, Previous: "1.0.0", Features: []dto.PluginFeature{}, Actions: []string{"upgrade", "rollback"}},
		Previous: "1.0.0"}, nil
}

func (f *fakePlugins) Rollback(_ context.Context, id string, restoreState bool, by string) (dto.Plugin, error) {
	err := f.record("rollback " + id + " " + map[bool]string{true: "restore", false: "keep"}[restoreState] + " " + by)
	return dto.Plugin{ID: id, Version: "1.0.0", State: "starting", Parts: []string{}, Roles: []string{}, Permissions: []dto.PluginPermission{},
		Features: []dto.PluginFeature{}, Actions: []string{"upgrade", "restart"}}, err
}

// An upgrade without approval answers 409 with the literal diff params; with
// the approval it answers the upgraded package; the package bytes and the
// administrator reach the package host.
func TestPluginUpgradeRouteWire(t *testing.T) {
	plugins := &fakePlugins{}
	rig := newPluginRig(t, plugins)
	rec := rig.do(rig.session, "POST", "/api/v1/plugins/acme_backup/upgrade", "v2 bytes")
	want := `{"code":"PLUGIN_APPROVAL_REQUIRED","error":"upgrading acme_backup needs approval","params":{"approve_hash":"sha256:22",` +
		`"capabilities_added":"host.setup@1","capabilities_removed":"","devices_added":"","devices_removed":"","features_added":"","features_removed":"",` +
		`"from_version":"1.0.0","host_setup_added":"","host_setup_removed":"","id":"acme_backup","isolation":"",` +
		`"permissions":"http:public,http:routes","permissions_added":"http:public=elevated","permissions_removed":"push:banners=low",` +
		`"roles":"","roles_added":"","roles_removed":"","routes_added":"POST /hooks public","routes_removed":"","ui_mode":"","version":"2.0.0"}}`
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("upgrade without approval: %d\n%s\nwant\n%s", rec.Code, rec.Body, want)
	}
	rec = rig.do(rig.session, "POST", "/api/v1/plugins/acme_backup/upgrade?approve=sha256:22", "v2 bytes")
	want = `{"plugin":{"id":"acme_backup","version":"2.0.0","enabled":false,"state":"running","restarts":0,"parts":[],"isolation":"","roles":[],` +
		`"permissions":[],"previous":"1.0.0","on_demand":false,"features":[],"actions":["upgrade","rollback"]},"previous":"1.0.0","auto_approved":false,"unchanged":false}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("approved upgrade: %d\n%s\nwant\n%s", rec.Code, rec.Body, want)
	}
	if plugins.body != "v2 bytes" {
		t.Fatalf("the package reached the host as %q", plugins.body)
	}
	rec = rig.do(rig.session, "POST", "/api/v1/plugins/acme_backup/rollback?restore_state=true", "")
	want = `{"id":"acme_backup","version":"1.0.0","enabled":false,"state":"starting","restarts":0,"parts":[],"isolation":"","roles":[],` +
		`"permissions":[],"on_demand":false,"features":[],"actions":["upgrade","restart"]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("rollback: %d\n%s\nwant\n%s", rec.Code, rec.Body, want)
	}
	rig.do(rig.session, "POST", "/api/v1/plugins/acme_backup/rollback", "")
	got := strings.Join(plugins.recorded(), "|")
	if got != "upgrade acme_backup  admin|upgrade acme_backup sha256:22 admin|rollback acme_backup restore admin|rollback acme_backup keep admin" {
		t.Fatalf("calls = %s", got)
	}
}

// Only an administrator's session upgrades or rolls back; a bad
// restore_state is refused before the host is asked.
func TestPluginUpgradeRoutesAccessAndValidation(t *testing.T) {
	plugins := &fakePlugins{}
	rig := newPluginRig(t, plugins)
	readToken, writeToken := rig.mint("read"), rig.mint("write")
	for _, path := range []string{"/api/v1/plugins/acme_backup/upgrade?approve=sha256:22", "/api/v1/plugins/acme_backup/rollback"} {
		for _, who := range []struct {
			name, token string
			want        int
		}{{"anonymous", "", 401}, {"member", rig.member, 403}, {"read token", readToken, 403}, {"write token", writeToken, 403}, {"session", rig.session, 200}} {
			if rec := rig.do(who.token, "POST", path, "pkg"); rec.Code != who.want {
				t.Errorf("POST %s as %s: %d %s, want %d", path, who.name, rec.Code, rec.Body, who.want)
			}
		}
		if rec := rig.do(rig.session, "GET", path, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d", path, rec.Code)
		}
	}
	before := len(plugins.recorded())
	rec := rig.do(rig.session, "POST", "/api/v1/plugins/acme_backup/rollback?restore_state=sometimes", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"restore_state"`) {
		t.Fatalf("bad restore_state: %d %s", rec.Code, rec.Body)
	}
	if len(plugins.recorded()) != before {
		t.Fatalf("the plugin host was asked: %v", plugins.recorded())
	}
}

// The upgrade and rollback refusals answer their status with code and
// params intact.
func TestPluginUpgradeRefusals(t *testing.T) {
	for _, tc := range []struct {
		path   string
		err    error
		status int
		body   string
	}{
		{"upgrade", messages.NewProductError(messages.ErrorPluginUpgradeIDMismatch, "the package is acme_other, not acme_backup",
			map[string]any{"id": "acme_backup", "package_id": "acme_other"}), 422,
			`{"code":"PLUGIN_UPGRADE_ID_MISMATCH","error":"the package is acme_other, not acme_backup","params":{"id":"acme_backup","package_id":"acme_other"}}`},
		{"upgrade", messages.NewProductError(messages.ErrorPluginUpgradeRolledBack, "rolled back", map[string]any{"id": "acme_backup", "version": "2.0.0",
			"previous": "1.0.0", "cause_code": "PLUGIN_CRASHED"}), 422,
			`{"code":"PLUGIN_UPGRADE_ROLLED_BACK","error":"rolled back","params":{"cause_code":"PLUGIN_CRASHED","id":"acme_backup","previous":"1.0.0","version":"2.0.0"}}`},
		{"upgrade", messages.NewProductError(messages.ErrorPluginSnapshotFailed, "no space", map[string]any{"id": "acme_backup"}), 500,
			`{"code":"PLUGIN_SNAPSHOT_FAILED","error":"no space","params":{"id":"acme_backup"}}`},
		{"upgrade", messages.NewProductError(messages.ErrorPluginUpgradeInProgress, "busy", map[string]any{"id": "acme_backup"}), 409,
			`{"code":"PLUGIN_UPGRADE_IN_PROGRESS","error":"busy","params":{"id":"acme_backup"}}`},
		{"upgrade", messages.NewProductError(messages.ErrorPluginNotInstalled, "no", map[string]any{"id": "acme_backup"}), 404,
			`{"code":"PLUGIN_NOT_INSTALLED","error":"no","params":{"id":"acme_backup"}}`},
		{"rollback", messages.NewProductError(messages.ErrorPluginNoPreviousVersion, "none", map[string]any{"id": "acme_backup"}), 409,
			`{"code":"PLUGIN_NO_PREVIOUS_VERSION","error":"none","params":{"id":"acme_backup"}}`},
		{"rollback", messages.NewProductError(messages.ErrorPluginSnapshotMissing, "none", map[string]any{"id": "acme_backup", "version": "1.0.0"}), 409,
			`{"code":"PLUGIN_SNAPSHOT_MISSING","error":"none","params":{"id":"acme_backup","version":"1.0.0"}}`},
		{"rollback", messages.NewProductError("PLUGIN_STATE_TOO_NEW", "too new", map[string]any{"marker": "plugin.acme_backup.data"}), 409,
			`{"code":"PLUGIN_STATE_TOO_NEW","error":"too new","params":{"marker":"plugin.acme_backup.data"}}`},
	} {
		rig := newPluginRig(t, &fakePlugins{err: tc.err})
		rec := rig.do(rig.session, "POST", "/api/v1/plugins/acme_backup/"+tc.path, "pkg")
		if rec.Code != tc.status || strings.TrimSpace(rec.Body.String()) != tc.body {
			t.Errorf("%s with %v: %d %s, want %d %s", tc.path, tc.err, rec.Code, rec.Body, tc.status, tc.body)
		}
	}
}
