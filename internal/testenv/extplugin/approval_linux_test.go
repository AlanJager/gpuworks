//go:build linux

package extplugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The 409 a first install answers carries every approval param on the
// wire, as a string, empty where nothing is listed: the key set the
// console reads.
func TestInstallRefusalWireParams(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	status, body := sv.do(http.MethodPost, "/api/v1/plugins/install", bytes.NewReader(s.fakePackage("acme_fake", "1.0.0")))
	var refusal struct {
		Code   string            `json:"code"`
		Params map[string]string `json:"params"`
	}
	if status != http.StatusConflict || json.Unmarshal(body, &refusal) != nil || refusal.Code != "PLUGIN_APPROVAL_REQUIRED" {
		t.Fatalf("install without approval = %d %s", status, body)
	}
	hash := refusal.Params["approve_hash"]
	if !extapi.Digest(hash).Valid() {
		t.Fatalf("approve_hash = %q", hash)
	}
	var capabilities []string
	for _, kind := range extapi.TableKinds() {
		capabilities = append(capabilities, extapi.TableCapability(kind)+"@1")
	}
	sort.Strings(capabilities)
	want := map[string]string{
		"id": "acme_fake", "version": "1.0.0", "approve_hash": hash,
		"permissions": "push:banners,push:node_holds", "roles": "availability_authority",
		"from_version":      "",
		"permissions_added": "push:banners=low,push:node_holds=high", "permissions_removed": "",
		"roles_added": "availability_authority", "roles_removed": "",
		"capabilities_added": strings.Join(capabilities, ","), "capabilities_removed": "",
		"features_added": "acme.backup,acme.tools", "features_removed": "",
		"routes_added": "", "routes_removed": "",
		"host_setup_added": "", "host_setup_removed": "",
		"devices_added": "", "devices_removed": "",
		"ui_mode": "", "isolation": "->" + extapi.IsolationLandlock,
	}
	if len(refusal.Params) != len(want) {
		t.Fatalf("params = %v\nwant %v", refusal.Params, want)
	}
	for k, v := range want {
		if got, ok := refusal.Params[k]; !ok || got != v {
			t.Fatalf("params[%q] = %q (present %v), want %q\nbody %s", k, got, ok, v, body)
		}
	}
}
