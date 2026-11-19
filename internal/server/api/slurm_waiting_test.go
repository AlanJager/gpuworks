package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func intp(n int) *int { return &n }

// waitingOf is the `waiting` of the entry with this name in the list.
func waitingOf(t *testing.T, rig *tokenRig, name string) map[string]any {
	t.Helper()
	list := decodeMap(t, rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "").Body.Bytes())
	for _, it := range list["items"].([]any) {
		m := it.(map[string]any)
		if m["name"] == name {
			w, _ := m["waiting"].(map[string]any)
			return w
		}
	}
	t.Fatalf("no entry %q in %v", name, list)
	return nil
}

// An entry with no node says why, from what the server saw: nothing run yet,
// the setup fetched and no node since, then nothing once a node asked. A fetch
// that only checks is not the setup having run.
func TestSlurmEntryWithoutNodesSaysWhatItIsWaitingFor(t *testing.T) {
	rig := newTokenRig(t)
	_, token, _ := createSlurm(t, rig, slurmCreateBody)
	if got := waitingOf(t, rig, "hpc"); !reflect.DeepEqual(got, map[string]any{"reason": "not_applied"}) {
		t.Fatalf("a new entry waiting = %v", got)
	}
	fetch := func(query string) {
		t.Helper()
		if rec := rig.do("", http.MethodGet, "/slurm/setup.sh?token="+url.QueryEscape(token)+query, ""); rec.Code != http.StatusOK {
			t.Fatalf("setup.sh%s = %d", query, rec.Code)
		}
	}
	fetch("&check=1")
	if got := waitingOf(t, rig, "hpc"); got["reason"] != "not_applied" {
		t.Fatalf("a fetch to check only = %v", got)
	}
	fetch("")
	// A moment after the setup ran no node has called, which is normal: the
	// health check reaches them within one to one and a half intervals.
	if got := waitingOf(t, rig, "hpc"); !reflect.DeepEqual(got, map[string]any{"reason": "joining"}) {
		t.Fatalf("the setup fetched, no node yet = %v", got)
	}
	// A node asking whether the token is still good is a node that reached the server.
	if rec := rig.do("", http.MethodGet, "/slurm/token-status?token="+url.QueryEscape(token), ""); rec.Code != http.StatusOK {
		t.Fatalf("token-status = %d", rec.Code)
	}
	if got := waitingOf(t, rig, "hpc"); got != nil {
		t.Fatalf("a node asked, waiting = %v", got)
	}
}

// An unknown token is not a node having called, and a revoked or an enrolled
// entry is not waiting at all.
func TestSlurmEntryThatIsRevokedOrHasNodesIsNotWaiting(t *testing.T) {
	rig := newTokenRig(t)
	uuid, _, _ := createSlurm(t, rig, slurmCreateBody)
	if rec := rig.do("", http.MethodGet, "/slurm/token-status?token=not-a-token", ""); rec.Code != http.StatusOK {
		t.Fatalf("unknown token-status = %d", rec.Code)
	}
	if got := waitingOf(t, rig, "hpc"); got["reason"] != "not_applied" {
		t.Fatalf("after an unknown token = %v", got)
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/revoke", ""); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if got := waitingOf(t, rig, "hpc"); got != nil {
		t.Fatalf("a revoked entry waiting = %v", got)
	}
	_, token2, _ := createSlurm(t, rig, `{"name":"hpc2","server_url":"https://gw.example:8800"}`)
	enroll := fmt.Sprintf(`{"token":%q,"slurm_cluster_name":"demo","node_name":"gpu000"}`, token2)
	if rec := rig.do("", http.MethodPost, "/api/v1/slurm/enroll", enroll); rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body)
	}
	if got := waitingOf(t, rig, "hpc2"); got != nil {
		t.Fatalf("an entry with a node waiting = %v", got)
	}
}

// The cloud's own count of compute nodes is read live: a cluster with none has
// nothing to join, whatever else was seen.
func TestSlurmEntryOfAClusterWithoutComputeNodesSaysSo(t *testing.T) {
	rig, drv := newHPCListRig(t)
	drv.set(func(d *hpcListDriver) {
		d.result = &provider.HPCClusterList{Clusters: []provider.HPCClusterListing{
			{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-9", Name: "empty", Scheduler: "slurm"}, Region: "cn-shenzhen", State: "running", Running: true, ComputeNodes: intp(0)},
			{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-1", Name: "busy", Scheduler: "slurm"}, Region: "cn-shenzhen", State: "running", Running: true, ComputeNodes: intp(3)},
			{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-8", Name: "unsaid", Scheduler: "slurm"}, Region: "cn-shenzhen", State: "running", Running: true},
		}}
	})
	for _, id := range []string{"ehpc-sz-9", "ehpc-sz-1", "ehpc-sz-8"} {
		rig.create(id)
	}
	if got := waitingOf(t, rig.tokenRig, "ehpc-ehpc-sz-9"); !reflect.DeepEqual(got, map[string]any{"reason": "no_compute_nodes"}) {
		t.Fatalf("empty cluster = %v", got)
	}
	// Nodes exist: the cloud does not say they are the reason, and nothing ran yet.
	if got := waitingOf(t, rig.tokenRig, "ehpc-ehpc-sz-1"); got["reason"] != "not_applied" {
		t.Fatalf("cluster with nodes = %v", got)
	}
	// A listing that does not count the nodes says nothing about them.
	if got := waitingOf(t, rig.tokenRig, "ehpc-ehpc-sz-8"); got["reason"] != "not_applied" {
		t.Fatalf("cluster with an unknown count = %v", got)
	}
}

func TestSlurmWaitingReasonOrder(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	ran := now.Add(-time.Minute)
	down := []provider.SlurmReachHost{{Role: "node", Name: "gpu000", Status: "failed", Reason: "timeout"}}
	for _, tc := range []struct {
		name    string
		seen    slurmSeen
		covered bool
		nodes   *int
		want    string
	}{
		{"nothing seen", slurmSeen{}, true, nil, "not_applied"},
		{"setup seen a minute ago", slurmSeen{setupAt: ran}, true, nil, "joining"},
		{"setup seen long ago", slurmSeen{setupAt: now.Add(-slurmJoinWindow)}, true, nil, "no_node_called"},
		{"a node called", slurmSeen{setupAt: ran, nodeCalled: true}, true, nil, ""},
		{"no compute nodes outranks the rest", slurmSeen{setupAt: ran, unreachable: down}, true, intp(0), "no_compute_nodes"},
		{"unreachable outranks no node called", slurmSeen{setupAt: ran, unreachable: down}, true, intp(2), "server_unreachable"},
		{"unreachable is a finding, not a guess", slurmSeen{unreachable: down}, false, nil, "server_unreachable"},
		{"before the server watched nothing is claimed", slurmSeen{}, false, nil, ""},
		{"before the server watched, a setup is not guessed either", slurmSeen{setupAt: ran}, false, intp(1), ""},
	} {
		if got, _ := slurmWaitingReason(tc.seen, tc.covered, tc.nodes, now); got != tc.want {
			t.Errorf("%s: reason = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An entry created before the server began watching is not claimed to have
// had no setup run; one created after is.
func TestSlurmActivityCoversOnlyEntriesCreatedAfterItBegan(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	var s slurmActivity
	s.now = func() time.Time { return now }
	s.begin()
	older := store.SlurmCluster{ID: 1, CreatedAt: now.Add(-time.Hour)}
	newer := store.SlurmCluster{ID: 2, CreatedAt: now.Add(time.Minute)}
	if _, covered := s.seen(older); covered {
		t.Fatal("an entry from before the server began is covered")
	}
	if _, covered := s.seen(newer); !covered {
		t.Fatal("an entry from after the server began is not covered")
	}
	s.setupRan(2)
	s.reached(2, provider.SlurmReach{Hosts: []provider.SlurmReachHost{{Role: "node", Name: "n", Status: "failed", Reason: "dns"}}})
	seen, _ := s.seen(newer)
	if !seen.setupSeen() || len(seen.unreachable) != 1 {
		t.Fatalf("seen = %+v", seen)
	}
	s.reached(2, provider.SlurmReach{Hosts: []provider.SlurmReachHost{{Role: "node", Name: "n", Status: "ok"}}})
	if seen, _ := s.seen(newer); len(seen.unreachable) != 0 {
		t.Fatalf("a later run that reached the server left %v", seen.unreachable)
	}
	s.forget(2)
	if seen, _ := s.seen(newer); seen.setupSeen() {
		t.Fatal("a deleted entry is remembered")
	}
}

// proberDriver is the apply account whose driver also asks the compute nodes
// itself (provider.SlurmNodeProber): the script it was given is recorded and
// each node answers as scripted.
type proberDriver struct {
	*applyDriver
	mu      sync.Mutex
	scripts []string
	runs    []provider.SlurmNodeRun
	err     error
}

func (d *proberDriver) Name() string { return "api-slurm-prober-provider" }

func (d *proberDriver) RunOnSlurmNodes(_ context.Context, _ provider.Credentials, region, clusterID, script string, _ time.Duration) ([]provider.SlurmNodeRun, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scripts = append(d.scripts, region+" "+clusterID+"\n"+script)
	return d.runs, d.err
}

func newProberRig(t *testing.T, manager string, nodes ...provider.SlurmNodeRun) (*applyRig, *proberDriver) {
	t.Helper()
	drv := &proberDriver{applyDriver: &applyDriver{answer: okRun(manager)}, runs: nodes}
	return newApplyRigFor(t, drv.applyDriver, drv), drv
}

const controllerReach = "GWKS_SLURM step=server_reach role=controller host=ctl status=ok\n"

// A check says, per host, whether the server's address can be reached: the
// controller from the manager's own run, each GPU node from the platform's,
// which asked through the cloud while the manager ran. A host that cannot
// makes the check partial with its own warning code.
func TestApplyCheckSaysWhichHostsCannotReachTheServer(t *testing.T) {
	rig, drv := newProberRig(t, checkOutput+controllerReach,
		provider.SlurmNodeRun{Name: "gpu000", InstanceID: "i-gpu000", Output: "GWKS_REACH rc=28\n"},
		provider.SlurmNodeRun{Name: "gpu001", InstanceID: "i-gpu001", Output: "GWKS_REACH rc=0\n"},
		provider.SlurmNodeRun{Name: "gpu002", InstanceID: "i-gpu002", NotAsked: provider.SlurmReachAssistantOffline})
	uuid, _ := rig.create("ehpc-sz-1")
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}
	got := decodeMap(t, rec.Body.Bytes())
	wantReach := map[string]any{"hosts": []any{
		map[string]any{"role": "controller", "name": "ctl", "status": "ok"},
		map[string]any{"role": "node", "name": "gpu000", "status": "failed", "reason": "timeout"},
		map[string]any{"role": "node", "name": "gpu001", "status": "ok"},
		map[string]any{"role": "node", "name": "gpu002", "status": "unknown", "reason": "assistant_offline"},
	}}
	if !reflect.DeepEqual(got["reach"], wantReach) {
		t.Fatalf("reach\n got %v\nwant %v", got["reach"], wantReach)
	}
	if !reflect.DeepEqual(got["warnings"], []any{"conf_include", "server_unreachable"}) || got["status"] != "partial" {
		t.Fatalf("status %v warnings %v", got["status"], got["warnings"])
	}
	// The manager asked only itself: the platform asks the nodes.
	if script := rig.drv.runs[0].script; !strings.Contains(script, "reach_flag='--no-node-reach'") {
		t.Fatalf("the manager script does not skip the nodes:\n%s", script)
	}
	if len(drv.scripts) != 1 || !strings.HasPrefix(drv.scripts[0], "cn-shenzhen ehpc-sz-1\n") || !strings.Contains(drv.scripts[0], "url='https://gw.example:8800/healthz'") {
		t.Fatalf("the nodes were asked with %q", drv.scripts)
	}
	// What a check found is what the entry says it waits for.
	w := waitingOf(t, rig.tokenRig, "ehpc-ehpc-sz-1")
	wantWaiting := map[string]any{"reason": "server_unreachable", "hosts": []any{map[string]any{"role": "node", "name": "gpu000", "status": "failed", "reason": "timeout"}}}
	if !reflect.DeepEqual(w, wantWaiting) {
		t.Fatalf("waiting = %v", w)
	}
}

// An apply carries the same answer, as a step of its own that needs attention,
// and a run that reached everywhere clears the entry's earlier finding.
func TestApplyOutcomeSaysWhichHostsCannotReachTheServerAndClearsWhenFixed(t *testing.T) {
	rig, drv := newProberRig(t, setupDoneOutput+controllerReach+"GWKS_APPLY gres=added node=gpu000\n",
		provider.SlurmNodeRun{Name: "gpu000", InstanceID: "i-gpu000", Output: "GWKS_REACH rc=6\n"})
	uuid, _ := rig.create("ehpc-sz-1")
	path := "/api/v1/slurm/clusters/" + uuid + "/apply"
	rec := rig.do(rig.session, http.MethodPost, path, `{"dry_run":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	got := decodeMap(t, rec.Body.Bytes())
	steps := got["steps"].([]any)
	if last := steps[len(steps)-1]; !reflect.DeepEqual(last, map[string]any{"id": "reach", "status": "warn"}) || got["status"] != "partial" {
		t.Fatalf("status %v last step %v", got["status"], last)
	}
	reach := got["reach"].(map[string]any)["hosts"].([]any)
	if len(reach) != 2 || !reflect.DeepEqual(reach[1], map[string]any{"role": "node", "name": "gpu000", "status": "failed", "reason": "dns"}) {
		t.Fatalf("reach = %v", reach)
	}
	if w := waitingOf(t, rig.tokenRig, "ehpc-ehpc-sz-1"); w["reason"] != "server_unreachable" {
		t.Fatalf("waiting = %v", w)
	}
	drv.mu.Lock()
	drv.runs = []provider.SlurmNodeRun{{Name: "gpu000", InstanceID: "i-gpu000", Output: "GWKS_REACH rc=0\n"}}
	drv.mu.Unlock()
	rec = rig.do(rig.session, http.MethodPost, path, `{"dry_run":false}`)
	got = decodeMap(t, rec.Body.Bytes())
	steps = got["steps"].([]any)
	if last := steps[len(steps)-1]; !reflect.DeepEqual(last, map[string]any{"id": "reach", "status": "pass"}) {
		t.Fatalf("last step %v", last)
	}
	// The setup ran and every host reaches the server: what is left is waiting
	// for a node to call, which it is allowed to take a while.
	if w := waitingOf(t, rig.tokenRig, "ehpc-ehpc-sz-1"); !reflect.DeepEqual(w, map[string]any{"reason": "joining"}) {
		t.Fatalf("waiting after a clean apply = %v", w)
	}
}

// A cloud that cannot list the cluster's nodes says why instead of failing the
// check: the key lacks the permission, or the listing failed.
func TestApplyCheckSaysWhyTheNodesWereNotAsked(t *testing.T) {
	rig, drv := newProberRig(t, checkOutput+controllerReach)
	drv.err = &provider.SlurmApplyPermissionError{Permission: "ehpc:ListNodes", Err: fmt.Errorf("denied")}
	uuid, _ := rig.create("ehpc-sz-1")
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("check = %d %s", rec.Code, rec.Body)
	}
	reach := decodeMap(t, rec.Body.Bytes())["reach"]
	want := map[string]any{"hosts": []any{map[string]any{"role": "controller", "name": "ctl", "status": "ok"}}, "nodes_unlisted": "permission_missing", "nodes_permission": "ehpc:ListNodes"}
	if !reflect.DeepEqual(reach, want) {
		t.Fatalf("reach\n got %v\nwant %v", reach, want)
	}
}

// exposedInstaller is an installer that also knows what the connector of
// `gpuworks expose` last observed.
type exposedInstaller struct {
	provision.Installer
	seen *dto.NodeConnectionExposure
}

func (e exposedInstaller) ConnectionExposure() *dto.NodeConnectionExposure { return e.seen }

// The platform's own quick tunnel is said on the defaults, so a form can warn
// before an entry is made with an address that changes; a stable address, a
// tunnel that is off or failed, and no observation at all say nothing.
func TestSlurmDefaultsSayTheQuickTunnelAddress(t *testing.T) {
	rig := newTokenRig(t)
	if got := decodeMap(t, rig.do(rig.session, http.MethodGet, "/api/v1/slurm/defaults", "").Body.Bytes())["quick_tunnel_url"]; got != "" {
		t.Fatalf("no tunnel: quick_tunnel_url = %v", got)
	}
	signer, err := auth.NewTokenSigner("quick-tunnel-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := rig.st.VerifyUser("admin", "gpuworks-test-password-2026")
	session, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: admin.ID, Username: admin.Username, Role: admin.Role})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		seen *dto.NodeConnectionExposure
		want string
	}{
		{"ready", &dto.NodeConnectionExposure{URL: "https://calm-river-1.trycloudflare.com/", State: "ready"}, "https://calm-river-1.trycloudflare.com"},
		{"degraded", &dto.NodeConnectionExposure{URL: "https://calm-river-1.trycloudflare.com", State: "degraded"}, "https://calm-river-1.trycloudflare.com"},
		{"disabled", &dto.NodeConnectionExposure{URL: "https://calm-river-1.trycloudflare.com", State: "disabled"}, ""},
		{"failed", &dto.NodeConnectionExposure{URL: "https://calm-river-1.trycloudflare.com", State: "failed"}, ""},
		{"a stable address", &dto.NodeConnectionExposure{URL: "https://gw.example.com", State: "ready"}, ""},
		{"a lookalike", &dto.NodeConnectionExposure{URL: "https://trycloudflare.com.evil.example", State: "ready"}, ""},
		{"no observation", nil, ""},
	} {
		rig.h = Router(Deps{Store: rig.st, AuthSigner: signer, NodeInstaller: exposedInstaller{seen: tc.seen}})
		got := decodeMap(t, rig.do(session, http.MethodGet, "/api/v1/slurm/defaults", "").Body.Bytes())["quick_tunnel_url"]
		if got != tc.want {
			t.Errorf("%s: quick_tunnel_url = %v, want %q", tc.name, got, tc.want)
		}
	}
}

// oldSlurmEntry is an entry created an hour ago, so older than
// slurmStalledAfter, and the handlers reading it with the clock of what the
// server saw in the returned time, which a test moves. watchedFrom is when the
// server began watching relative to now.
func oldSlurmEntry(t *testing.T, watchedFrom time.Duration) (*apiHandlers, *store.SlurmCluster, *time.Time) {
	t.Helper()
	rig := newTokenRig(t)
	created, err := rig.st.CreateSlurmCluster(store.SlurmCluster{Name: "old", ServerURL: "https://gw", RunAs: "root",
		InstallDir: store.SlurmDefaultInstallDir, ScriptDir: store.SlurmDefaultScriptDir,
		IdleReturnSeconds: store.SlurmDefaultIdleReturnSeconds, PendingTimeoutSeconds: store.SlurmDefaultPendingTimeoutSeconds})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	h := &apiHandlers{d: Deps{Store: rig.st}}
	h.slurmActivity.now = func() time.Time { return now }
	h.slurmActivity.started = now.Add(watchedFrom)
	if err := rig.st.DB().Model(&store.SlurmCluster{}).Where("id = ?", created.ID).Update("created_at", now.Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	c, err := rig.st.SlurmClusterByID(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return h, c, &now
}

func slurmEntryView(t *testing.T, h *apiHandlers, c *store.SlurmCluster) dto.SlurmCluster {
	t.Helper()
	views, err := h.slurmViews(context.Background(), []store.SlurmCluster{*c})
	if err != nil {
		t.Fatal(err)
	}
	return slurmClusterDTO(*c, views[c.ID], false)
}

// Nodes join within one to one and a half health-check intervals of the
// setup, so right after it an entry with no node yet is joining and not
// stalled however old it is; once the window has passed, silence is a finding.
func TestSlurmEntryJoiningWindowHoldsBackNoNodeCalledAndStalled(t *testing.T) {
	h, c, now := oldSlurmEntry(t, -48*time.Hour)
	view := func() dto.SlurmCluster { return slurmEntryView(t, h, c) }
	if got := view(); got.State != "stalled" || got.Waiting == nil || got.Waiting.Reason != "not_applied" {
		t.Fatalf("an old entry never set up = %s %+v", got.State, got.Waiting)
	}
	h.slurmActivity.setupRan(c.ID)
	for _, after := range []time.Duration{0, time.Minute, slurmJoinWindow - time.Minute - time.Second} {
		*now = now.Add(after)
		got := view()
		if got.Stalled || got.State != "pending" || got.Waiting == nil || got.Waiting.Reason != "joining" {
			t.Fatalf("%v after the setup = stalled %v, state %s, waiting %+v", after, got.Stalled, got.State, got.Waiting)
		}
	}
	*now = now.Add(2 * time.Second)
	if got := view(); !got.Stalled || got.State != "stalled" || got.Waiting == nil || got.Waiting.Reason != "no_node_called" {
		t.Fatalf("after the window = stalled %v, state %s, waiting %+v", got.Stalled, got.State, got.Waiting)
	}
	// Running the setup again starts the window again.
	h.slurmActivity.setupRan(c.ID)
	if got := view(); got.Stalled || got.Waiting == nil || got.Waiting.Reason != "joining" {
		t.Fatalf("after a second run = stalled %v, waiting %+v", got.Stalled, got.Waiting)
	}
	// A node that called ends the wait whatever the window says.
	h.slurmActivity.nodeAsked(c.ID)
	if got := view(); got.Waiting != nil {
		t.Fatalf("a node called = %+v", got.Waiting)
	}
}

// An entry created before the server began watching is not claimed to be
// joining: nothing says when its setup ran.
func TestSlurmEntryFromBeforeTheServerWatchedIsNotJoining(t *testing.T) {
	h, c, _ := oldSlurmEntry(t, time.Hour)
	h.slurmActivity.setupRan(c.ID)
	if got := slurmEntryView(t, h, c); got.Waiting != nil || !got.Stalled {
		t.Fatalf("an uncovered entry = stalled %v, waiting %+v", got.Stalled, got.Waiting)
	}
}
