package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
)

// The cluster form takes its defaults, limits and vendors from here and keeps
// no copy of them. The wire body is pinned literally: every key, in order.
func TestKubeDefaultsDictionaryOnTheWire(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/defaults", "")
	want := `{"vendor":"nvidia","namespace":"gpuworks-system","state_dir":"/opt/gpuworks","engine_ports":"40000-42767",` +
		`"idle_return_seconds":60,"idle_return_keep":-1,"agent_image":"gpuworks/gpuworks-agent:` + version.Version + `","stalled_after_seconds":300,` +
		`"vendors":[{"id":"nvidia","runtime_class":true},{"id":"ascend","runtime_class":false},{"id":"hygon","runtime_class":false},{"id":"ppu","runtime_class":false}]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("defaults = %d %s\nwant %s", rec.Code, rec.Body, want)
	}
	if rec := rig.do("", http.MethodGet, "/api/v1/kube/defaults", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous defaults = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/defaults", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST defaults = %d", rec.Code)
	}
}

// What the dictionary says is what a request that leaves a field empty gets.
func TestKubeDefaultsAreWhatAnEmptyRequestTakes(t *testing.T) {
	rig := newTokenRig(t)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"bare","server_url":"https://gw.example"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	got := decodeKube[map[string]any](t, rec)
	dict := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodGet, "/api/v1/kube/defaults", ""))
	for _, key := range []string{"vendor", "namespace", "state_dir", "engine_ports", "idle_return_seconds", "agent_image"} {
		if got[key] != dict[key] {
			t.Errorf("%s: a bare request got %v, the dictionary says %v", key, got[key], dict[key])
		}
	}
	// The vendors that take no RuntimeClass drop one; the dictionary names
	// which they are.
	for _, v := range dict["vendors"].([]any) {
		vendor := v.(map[string]any)
		body := fmt.Sprintf(`{"name":"rc-%s","server_url":"https://gw.example","vendor":%q,"runtime_class":"special"}`, vendor["id"], vendor["id"])
		created := decodeKube[map[string]any](t, rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", body))
		if applies := vendor["runtime_class"].(bool); applies != (created["runtime_class"] == "special") {
			t.Errorf("%v: dictionary runtime_class=%v but the cluster kept %q", vendor["id"], applies, created["runtime_class"])
		}
	}
}

// A cluster no node joined for longer than the dictionary's stalled_after is
// stalled; revoked clusters and ones a node joined never are.
func TestKubeClusterStalledIsComputedByTheServer(t *testing.T) {
	rig := newTokenRig(t)
	make := func(name string) store.KubeCluster {
		c, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return *c
	}
	age := func(c store.KubeCluster, by time.Duration) {
		if err := rig.st.DB().Model(&store.KubeCluster{}).Where("id = ?", c.ID).Update("created_at", time.Now().Add(-by)).Error; err != nil {
			t.Fatal(err)
		}
	}
	fresh := make("fresh")
	waiting := make("waiting")
	age(waiting, kubeStalledAfter+time.Minute)
	almost := make("almost")
	age(almost, kubeStalledAfter-time.Minute)
	enrolled := make("enrolled")
	age(enrolled, time.Hour)
	if _, _, err := rig.st.EnrollKubeNode(enrolled.Token, "uid-1", "gpu-1"); err != nil {
		t.Fatal(err)
	}
	revoked := make("revoked")
	age(revoked, time.Hour)
	if err := rig.st.RevokeKubeCluster(revoked.UUID); err != nil {
		t.Fatal(err)
	}

	rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	var list struct {
		Items []struct {
			Name    string `json:"name"`
			Stalled bool   `json:"stalled"`
		} `json:"items"`
	}
	list = decodeKube[struct {
		Items []struct {
			Name    string `json:"name"`
			Stalled bool   `json:"stalled"`
		} `json:"items"`
	}](t, rec)
	got := map[string]bool{}
	for _, it := range list.Items {
		got[it.Name] = it.Stalled
	}
	want := map[string]bool{"fresh": false, "waiting": true, "almost": false, "enrolled": false, "revoked": false}
	for name, stalled := range want {
		if has, ok := got[name]; !ok || has != stalled {
			t.Errorf("%s: stalled = %v (listed %v), want %v; body %s", name, has, ok, stalled, rec.Body)
		}
	}
	_ = fresh
}

// Whether the server can apply or remove an entry is its own word, with the
// reason: by hand has no way in, a private API server cannot be reached, a
// revoked entry can only be removed. A cloud that cannot be asked leaves the
// entry applicable (the operation reports the failure), and the cloud is asked
// once for however many polls.
func TestKubeClusterOperationsAreDecidedByTheServer(t *testing.T) {
	rig := newKubeSourceRig(t)
	create := func(name, body string) map[string]any {
		t.Helper()
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters", `{"name":"`+name+`","server_url":"https://gw.example"`+body+`}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", name, rec.Code, rec.Body)
		}
		return decodeKube[map[string]any](t, rec)
	}
	from := func(cluster string) string {
		return fmt.Sprintf(`,"provider_id":%d,"provider_cluster_id":%q,"provider_pool_ids":[%q]`, rig.provider, cluster, map[string]string{"c-gpu": "np-1", "c-private": "np-p", "c-other": "np-o"}[cluster])
	}
	pub := create("pub", from("c-gpu"))
	priv := create("priv", from("c-private"))
	hand := create("hand", "")
	gone := create("revoked-pub", from("c-other"))
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters/"+gone["uuid"].(string)+"/revoke", ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}

	ops := func(c map[string]any) string {
		return fmt.Sprintf("apply=%v remove=%v blocked=%v", c["can_apply"], c["can_remove"], c["operation_blocked"])
	}
	if got := ops(pub); got != "apply=true remove=true blocked=<nil>" {
		t.Errorf("a cluster with a public API: %s", got)
	}
	if got := ops(priv); got != "apply=false remove=false blocked=private_api" {
		t.Errorf("a cluster the cloud says is private, as created: %s", got)
	}
	if got := ops(hand); got != "apply=false remove=false blocked=no_source" {
		t.Errorf("an entry made by hand: %s", got)
	}

	rec := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	var list struct {
		Items []map[string]any `json:"items"`
	}
	list = decodeKube[struct {
		Items []map[string]any `json:"items"`
	}](t, rec)
	byName := map[string]string{}
	for _, it := range list.Items {
		byName[it["name"].(string)] = ops(it)
	}
	for name, want := range map[string]string{
		"pub":         "apply=true remove=true blocked=<nil>",
		"priv":        "apply=false remove=false blocked=private_api",
		"hand":        "apply=false remove=false blocked=no_source",
		"revoked-pub": "apply=false remove=true blocked=revoked",
	} {
		if byName[name] != want {
			t.Errorf("list %s: %s, want %s", name, byName[name], want)
		}
	}

	before := rig.drv.lists.Load()
	for i := 0; i < 5; i++ {
		rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	}
	if after := rig.drv.lists.Load(); after != before {
		t.Errorf("five more polls asked the cloud %d more times; its answer is kept for a minute", after-before)
	}
}

func TestKubeClusterOperationsWhenTheCloudCannotBeAsked(t *testing.T) {
	rig := newKubeSourceRig(t)
	// An entry made while the cloud answered, read while it does not.
	provider := rig.provider
	if _, err := rig.st.CreateKubeCluster(store.KubeCluster{Name: "unsure", Namespace: "gpuworks-system", ProviderID: &provider, ProviderClusterID: "c-private"},
		store.KubeClusterPool{PoolID: "np-p", LabelKey: "alibabacloud.com/nodepool-id"}); err != nil {
		t.Fatal(err)
	}
	rig.drv.listErr = errors.New("aliyun: Forbidden.RAM")
	list := rig.do(rig.session, http.MethodGet, "/api/v1/kube/clusters", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"can_apply":true,"can_remove":true`) || strings.Contains(list.Body.String(), "operation_blocked") {
		t.Fatalf("without the cloud's word the entry stays applicable: %d %s", list.Code, list.Body)
	}
	// Making an entry needs the cloud: its pools are checked against it.
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/kube/clusters",
		fmt.Sprintf(`{"name":"new","server_url":"https://gw.example","provider_id":%d,"provider_cluster_id":"c-gpu","provider_pool_ids":["np-1"]}`, rig.provider))
	if rec.Code != http.StatusFailedDependency || !strings.Contains(rec.Body.String(), "Forbidden.RAM") {
		t.Fatalf("create while the cloud cannot be asked = %d %s", rec.Code, rec.Body)
	}
}
