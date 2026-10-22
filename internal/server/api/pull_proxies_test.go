package api

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Proxy entries: created after every other, the password never answered back
// (has_password only), kept by an update that leaves it out and removed by an
// empty one; each write sends the online nodes their proxy again, network
// only, never a full replay.
func TestPullProxiesCRUDWire(t *testing.T) {
	rig := newPullRig(t)
	rig.node("online-a", true, map[string]string{"site": "idc-sh"})
	rig.node("online-b", true, nil)
	rig.node("offline", false, nil)
	rig.want("GET", "/api/v1/pull/proxies", "", 200, `{"items":[],"total":0}`)

	rig.want("POST", "/api/v1/pull/proxies",
		`{"http_proxy":" proxy.corp:3128 ","https_proxy":"HTTP://Proxy.Corp:3128","no_proxy":" files.corp , 10.0.0.0/8 ,","username":" alice ","password":"s3cret","control_plane":true,"note":" office ","node_requirements":{"site":["idc-sh"]}}`, 201,
		`{"id":1,"position":0,"http_proxy":"http://proxy.corp:3128","https_proxy":"http://proxy.corp:3128","no_proxy":"files.corp,10.0.0.0/8","username":"alice","has_password":true,"control_plane":true,"note":"office","node_requirements":{"site":["idc-sh"]},"matched_nodes":1}`)
	if proxies, _ := rig.st.PullProxies(); len(proxies) != 1 || proxies[0].Password != "s3cret" || proxies[0].NetworkConfig().HTTPProxy != "http://alice:s3cret@proxy.corp:3128" {
		t.Fatalf("stored entry = %+v", proxies)
	}
	if _, body := rig.do(rig.admin, "GET", "/api/v1/pull/proxies", ""); strings.Contains(body, "s3cret") {
		t.Fatalf("the list repeats the password: %s", body)
	}
	synced := rig.networkSyncedNodes()
	sort.Strings(synced)
	if !reflect.DeepEqual(synced, []string{"online-a", "online-b"}) || len(rig.syncedNodes()) != 0 {
		t.Fatalf("network syncs = %v, full syncs = %v; want the online nodes, network only", synced, rig.syncedNodes())
	}

	rig.want("POST", "/api/v1/pull/proxies", `{"https_proxy":"socks5h://gw.corp:1080"}`, 201,
		`{"id":2,"position":1,"http_proxy":"","https_proxy":"socks5h://gw.corp:1080","no_proxy":"","username":"","has_password":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":3}`)

	// An update that leaves the password out keeps it; an empty one removes it.
	rig.want("PUT", "/api/v1/pull/proxies/1", `{"http_proxy":"http://proxy.corp:3129","username":"alice"}`, 200,
		`{"id":1,"position":0,"http_proxy":"http://proxy.corp:3129","https_proxy":"","no_proxy":"","username":"alice","has_password":true,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":3}`)
	rig.want("PUT", "/api/v1/pull/proxies/1", `{"http_proxy":"http://proxy.corp:3129","username":"","password":""}`, 200,
		`{"id":1,"position":0,"http_proxy":"http://proxy.corp:3129","https_proxy":"","no_proxy":"","username":"","has_password":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":3}`)

	rig.want("PUT", "/api/v1/pull/proxies/order", `{"ids":[2,1]}`, 200, `{"total":2,"items":[
		{"id":2,"position":0,"http_proxy":"","https_proxy":"socks5h://gw.corp:1080","no_proxy":"","username":"","has_password":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":3},
		{"id":1,"position":1,"http_proxy":"http://proxy.corp:3129","https_proxy":"","no_proxy":"","username":"","has_password":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":3}]}`)
	rig.want("DELETE", "/api/v1/pull/proxies/2", "", 204, "")
	rig.want("GET", "/api/v1/pull/proxies", "", 200, `{"total":1,"items":[
		{"id":1,"position":1,"http_proxy":"http://proxy.corp:3129","https_proxy":"","no_proxy":"","username":"","has_password":false,"control_plane":false,"note":"","node_requirements":{},"matched_nodes":3}]}`)
	if len(rig.syncedNodes()) != 0 {
		t.Fatalf("proxy edits triggered full syncs: %v", rig.syncedNodes())
	}
}

func TestPullProxyValidationAndMissingRows(t *testing.T) {
	rig := newPullRig(t)
	for name, tc := range map[string]struct{ body, field string }{
		"no proxy at all":            {`{"no_proxy":"files.corp"}`, "http_proxy"},
		"a login in the address":     {`{"http_proxy":"http://alice:secret@proxy.corp:3128"}`, "http_proxy"},
		"an address with a path":     {`{"https_proxy":"http://proxy.corp:3128/x"}`, "https_proxy"},
		"an unknown scheme":          {`{"http_proxy":"ftp://proxy.corp:21"}`, "http_proxy"},
		"a password and no user":     {`{"http_proxy":"http://proxy.corp:3128","password":"secret"}`, "username"},
		"a no_proxy with whitespace": {`{"http_proxy":"http://proxy.corp:3128","no_proxy":"a b"}`, "no_proxy"},
		"an invalid requirement key": {`{"http_proxy":"http://proxy.corp:3128","node_requirements":{"bad key!":["x"]}}`, "requirements"},
	} {
		code, body := rig.do(rig.admin, "POST", "/api/v1/pull/proxies", tc.body)
		var got struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s: %v %s", name, err, body)
		}
		if code != 400 || got.Code != "PULL_SETTING_INVALID" || got.Params["field"] != tc.field {
			t.Errorf("%s: %d %s, want 400 PULL_SETTING_INVALID on %q", name, code, body, tc.field)
		}
		if strings.Contains(body, "secret") {
			t.Errorf("%s: the error repeats the password: %s", name, body)
		}
	}
	if proxies, _ := rig.st.PullProxies(); len(proxies) != 0 {
		t.Fatalf("a refused write stored an entry: %+v", proxies)
	}
	for _, route := range []struct{ method, path, body string }{
		{"PUT", "/api/v1/pull/proxies/9", `{"http_proxy":"http://proxy.corp:3128"}`},
		{"DELETE", "/api/v1/pull/proxies/9", ``},
	} {
		if code, body := rig.do(rig.admin, route.method, route.path, route.body); code != 404 {
			t.Errorf("%s %s = %d %s, want 404", route.method, route.path, code, body)
		}
	}
	if code, body := rig.do(rig.admin, "PUT", "/api/v1/pull/proxies/order", `{"ids":[7]}`); code != 400 || !strings.Contains(body, `"field":"order"`) {
		t.Fatalf("a wrong order = %d %s", code, body)
	}
	if code, _ := rig.do(rig.admin, "POST", "/api/v1/pull/proxies", `{"http_proxy":"http://p:1","surprise":1}`); code != 400 {
		t.Fatalf("an unknown field = %d", code)
	}
}

// A node uses the first entry that selects it; the preview of a node and its
// detail read the same answer.
func TestNodePullProxyIsTheFirstEntryThatSelectsIt(t *testing.T) {
	rig := newPullRig(t)
	rig.node("idc", true, map[string]string{"site": "idc-sh"})
	rig.node("office", true, nil)
	for _, body := range []string{
		`{"http_proxy":"http://idc-proxy:3128","node_requirements":{"site":["idc-sh"]}}`,
		`{"http_proxy":"http://everyone:3128"}`,
		`{"http_proxy":"http://never-reached:3128","node_requirements":{"site":["idc-sh"]}}`,
	} {
		if code, text := rig.do(rig.admin, "POST", "/api/v1/pull/proxies", body); code != 201 {
			t.Fatalf("create = %d %s", code, text)
		}
	}
	for node, want := range map[string]string{"idc": "http://idc-proxy:3128", "office": "http://everyone:3128"} {
		var got struct {
			Proxy *struct {
				HTTPProxy string `json:"http_proxy"`
			} `json:"proxy"`
		}
		code, body := rig.do(rig.member, "GET", "/api/v1/nodes/"+node+"/pull-proxy", "")
		if code != 200 || json.Unmarshal([]byte(body), &got) != nil || got.Proxy == nil || got.Proxy.HTTPProxy != want {
			t.Fatalf("%s: %d %s, want %s", node, code, body, want)
		}
	}
	if code, _ := rig.do(rig.admin, "GET", "/api/v1/nodes/ghost/pull-proxy", ""); code != 404 {
		t.Fatalf("unknown node = %d", code)
	}
	if code, body := rig.do(rig.admin, "DELETE", "/api/v1/pull/proxies/2", ""); code != 204 {
		t.Fatalf("delete = %d %s", code, body)
	}
	if _, body := rig.do(rig.admin, "GET", "/api/v1/nodes/office/pull-proxy", ""); !strings.Contains(body, `"proxy":null`) {
		t.Fatalf("a node no entry selects = %s, want no proxy", body)
	}
}
