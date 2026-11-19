package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// applyDriver is an Alibaba Cloud account holding an E-HPC Slurm cluster
// (two GPU compute nodes and a CPU one) and a PBS cluster, whose manager node
// runs what the platform sends it: the script is recorded and the answer is
// scripted.
type applyDriver struct {
	apiInventoryDriver
	mu     sync.Mutex
	runs   []applyRun
	answer func(script string) (provider.SlurmManagerRun, error)
}

type applyRun struct {
	region, cluster, script string
	timeout                 time.Duration
}

func (d *applyDriver) Name() string { return "api-slurm-apply-provider" }

func (d *applyDriver) RunOnSlurmManager(_ context.Context, _ provider.Credentials, region, clusterID, script string, timeout time.Duration) (provider.SlurmManagerRun, error) {
	d.mu.Lock()
	d.runs = append(d.runs, applyRun{region, clusterID, script, timeout})
	d.mu.Unlock()
	return d.answer(script)
}

// plainDriver is an account whose driver cannot run commands on a manager node.
type plainDriver struct{ apiInventoryDriver }

func (d *plainDriver) Name() string { return "api-slurm-plain-provider" }

const setupDoneOutput = "Slurm cluster 'demo', slurm.conf at /etc/slurm/slurm.conf.\nGWKS_SLURM step=precheck status=ok cluster=demo\n" +
	"GWKS_SLURM step=check_script status=ok path=/opt/gpuworks-slurm/check.sh\nGWKS_SLURM step=token_file status=ok path=/opt/gpuworks-slurm/token\n" +
	"GWKS_SLURM step=shared status=yes fstype=nfs4\nGWKS_SLURM step=distribute_summary ok=0 failed=none skipped=shared\n" +
	"GWKS_SLURM step=slurm_conf status=ok path=/etc/slurm/slurm.conf backup=/etc/slurm/slurm.conf.gpuworks-1.bak chained=none interval=300\n" +
	"GWKS_SLURM step=configless value=yes\nGWKS_SLURM step=reconfigure status=ok\nGWKS_SLURM step=done status=ok\nGWKS_APPLY setup_exit=0\n"

func ehpcNode(id, host, cluster, scheduler string, gpus int) provider.Instance {
	inst := inventoryInstance(id, cluster+"_"+host, "")
	inst.GPUCount, inst.Region = gpus, "cn-shenzhen"
	inst.HPC = &provider.HPCCluster{ID: cluster, Name: cluster + "-name", Scheduler: scheduler, SchedulerVersion: "22.05.8"}
	return inst
}

type applyRig struct {
	*tokenRig
	drv        *applyDriver
	providerID uint
}

func newApplyRig(t *testing.T, answer func(script string) (provider.SlurmManagerRun, error)) *applyRig {
	t.Helper()
	drv := &applyDriver{answer: answer}
	return newApplyRigFor(t, drv, drv)
}

// newApplyRigFor is newApplyRig with the account's driver, which wraps drv.
func newApplyRigFor(t *testing.T, drv *applyDriver, registered provider.Driver) *applyRig {
	t.Helper()
	drv.instances = map[string]provider.Instance{
		"i-gpu000": ehpcNode("i-gpu000", "gpu000", "ehpc-sz-1", "slurm", 1),
		"i-gpu001": ehpcNode("i-gpu001", "gpu001", "ehpc-sz-1", "slurm", 4),
		"i-cpu000": ehpcNode("i-cpu000", "cpu000", "ehpc-sz-1", "slurm", 0),
		"i-pbs000": ehpcNode("i-pbs000", "comp000", "ehpc-sz-2", "pbs", 1),
		"i-plain":  inventoryInstance("i-plain", "plain", "203.0.113.21"),
	}
	provider.Register(registered)
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	admin, _ := st.VerifyUser("admin", "gpuworks-test-password-2026")
	box, err := auth.NewSecretbox(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	svc := provider.NewService(st, box)
	p, err := svc.Add(context.Background(), registered.Name(), "ali", provider.Credentials{APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("slurm-apply-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	session, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: admin.ID, Username: admin.Username, Role: admin.Role})
	if err != nil {
		t.Fatal(err)
	}
	rig := &tokenRig{t: t, h: Router(Deps{Store: st, Providers: svc, AuthSigner: signer}), st: st, session: session}
	if rec := rig.do(session, http.MethodPost, fmt.Sprintf("/api/v1/providers/%d/sync", p.ID), ""); rec.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", rec.Code, rec.Body)
	}
	return &applyRig{tokenRig: rig, drv: drv, providerID: p.ID}
}

func (r *applyRig) create(cluster string) (uuid string, entry map[string]any) {
	r.t.Helper()
	body := fmt.Sprintf(`{"name":"ehpc-%s","server_url":"https://gw.example:8800","provider_id":%d,"provider_cluster_id":%q}`, cluster, r.providerID, cluster)
	rec := r.do(r.session, http.MethodPost, "/api/v1/slurm/clusters", body)
	if rec.Code != http.StatusCreated {
		r.t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	entry = decodeMap(r.t, rec.Body.Bytes())
	return entry["uuid"].(string), entry
}

func okRun(output string) func(string) (provider.SlurmManagerRun, error) {
	return func(string) (provider.SlurmManagerRun, error) {
		return provider.SlurmManagerRun{ManagerInstance: "i-mgr", Output: output}, nil
	}
}

func TestSlurmEntryFromACloudClusterSaysWhatThePlatformCanDo(t *testing.T) {
	rig := newApplyRig(t, okRun(""))
	_, entry := rig.create("ehpc-sz-1")
	want := map[string]any{"provider_id": float64(rig.providerID), "provider_name": "ali", "provider_kind": rig.drv.Name(), "provider_cluster_id": "ehpc-sz-1"}
	if !reflect.DeepEqual(entry["source"], want) || entry["can_apply"] != true {
		t.Fatalf("source %v, can_apply %v", entry["source"], entry["can_apply"])
	}
	// A PBS cluster, an unsynced one and an entry without a source cannot be applied.
	_, pbs := rig.create("ehpc-sz-2")
	_, unknown := rig.create("ehpc-sz-9")
	_, plain, _ := createSlurm(t, rig.tokenRig, slurmCreateBody)
	_ = plain
	if pbs["can_apply"] != false || unknown["can_apply"] != false {
		t.Fatalf("can_apply: pbs %v, unknown %v", pbs["can_apply"], unknown["can_apply"])
	}
	list := decodeMap(t, rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "").Body.Bytes())
	for _, it := range list["items"].([]any) {
		m := it.(map[string]any)
		if m["name"] == "hpc" && (m["can_apply"] != false || m["source"] != nil) {
			t.Fatalf("an entry without a source: can_apply %v source %v", m["can_apply"], m["source"])
		}
	}
	// The account's instance list leaves the cluster's nodes out: they are the cluster's.
	rec := rig.do(rig.session, http.MethodGet, fmt.Sprintf("/api/v1/providers/%d/instances", rig.providerID), "")
	if strings.Contains(rec.Body.String(), "i-gpu000") || !strings.Contains(rec.Body.String(), "i-plain") {
		t.Fatalf("instances = %s", rec.Body)
	}
}

func TestApplySlurmClusterRunsTheSetupOnTheManagerAndAnswersTheOutcome(t *testing.T) {
	out := "GWKS_APPLY gres=added node=gpu000\nGWKS_APPLY gres=present node=gpu001\n" + setupDoneOutput + "GWKS_APPLY resume=ok node=gpu000\n"
	rig := newApplyRig(t, okRun(out))
	uuid, _ := rig.create("ehpc-sz-1")
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	got := decodeMap(t, rec.Body.Bytes())
	cluster := got["cluster"].(map[string]any)
	delete(got, "cluster")
	want := map[string]any{
		"status": "ok", "manager_instance": "i-mgr",
		"steps": []any{
			map[string]any{"id": "gres", "status": "pass"}, map[string]any{"id": "check_script", "status": "pass"},
			map[string]any{"id": "distribute", "status": "skip"}, map[string]any{"id": "slurm_conf", "status": "pass"},
			map[string]any{"id": "reconfigure", "status": "pass"}, map[string]any{"id": "resume", "status": "pass"},
		},
		"gres":          map[string]any{"added": []any{"gpu000"}, "present": []any{"gpu001"}, "no_gpu_gres": []any{}},
		"distribute":    map[string]any{"shared": true, "ok": float64(0), "failed": []any{}},
		"resumed_nodes": []any{"gpu000"}, "resume_failed_nodes": []any{}, "reach": map[string]any{"hosts": []any{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outcome\n got %v\nwant %v", got, want)
	}
	if cluster["uuid"] != uuid || cluster["setup_command"] == nil {
		t.Fatalf("the outcome's entry = %v", cluster)
	}
	if len(rig.drv.runs) != 1 {
		t.Fatalf("runs = %d", len(rig.drv.runs))
	}
	run := rig.drv.runs[0]
	token := strings.TrimSuffix(strings.TrimPrefix(cluster["setup_command"].(string), "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token="), "' | sudo sh")
	token, _ = url.QueryUnescape(token)
	// The compute nodes the account lists with a card, by their Slurm names;
	// the CPU node and the other cluster's node are not given a line. The
	// setup script is fetched through a one-time URL, not the entry's token.
	for _, w := range []string{"mode='setup'", "expect_gpu='gpu000\ngpu001'", "setup_url='https://gw.example:8800/slurm/setup.sh?once=", "gres='/etc/slurm/gres.conf'"} {
		if !strings.Contains(run.script, w) {
			t.Fatalf("script lacks %q:\n%s", w, run.script)
		}
	}
	if strings.Contains(run.script, token) || strings.Contains(run.script, "token=") {
		t.Fatalf("the command sent to the cloud carries the entry's token:\n%s", run.script)
	}
	if strings.Contains(run.script, "cpu000") || strings.Contains(run.script, "comp000") {
		t.Fatal("the script names a node that has no card or belongs to another cluster")
	}
	if run.region != "cn-shenzhen" || run.cluster != "ehpc-sz-1" || run.timeout != provider.SlurmApplyTimeout {
		t.Fatalf("run = %+v", run)
	}
}

func TestApplySlurmClusterReportsPartial(t *testing.T) {
	out := "GWKS_APPLY gres=added node=gpu000\nGWKS_APPLY gres=nogres node=gpu001\n" + strings.Replace(strings.Replace(setupDoneOutput,
		"step=shared status=yes fstype=nfs4", "step=shared status=no fstype=ext4", 1), "ok=0 failed=none skipped=shared", "ok=1 failed=gpu001", 1) + "GWKS_APPLY resume=failed node=gpu000\n"
	rig := newApplyRig(t, okRun(out))
	uuid, _ := rig.create("ehpc-sz-1")
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":false}`)
	got := decodeMap(t, rec.Body.Bytes())
	if rec.Code != http.StatusOK || got["status"] != "partial" {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	steps := map[string]string{}
	for _, s := range got["steps"].([]any) {
		steps[s.(map[string]any)["id"].(string)] = s.(map[string]any)["status"].(string)
	}
	if steps["gres"] != "warn" || steps["distribute"] != "warn" || steps["resume"] != "warn" || steps["slurm_conf"] != "pass" {
		t.Fatalf("steps = %v", steps)
	}
	if d := got["distribute"].(map[string]any); d["shared"] != false || d["ok"] != float64(1) || !reflect.DeepEqual(d["failed"], []any{"gpu001"}) {
		t.Fatalf("distribute = %v", d)
	}
	if g := got["gres"].(map[string]any); !reflect.DeepEqual(g["no_gpu_gres"], []any{"gpu001"}) {
		t.Fatalf("gres = %v", g)
	}
	if !reflect.DeepEqual(got["resume_failed_nodes"], []any{"gpu000"}) {
		t.Fatalf("resume_failed_nodes = %v", got["resume_failed_nodes"])
	}
}

func TestApplySlurmClusterRefusesWhatThePlatformCannotDo(t *testing.T) {
	rig := newApplyRig(t, okRun(setupDoneOutput))
	noSource, _, _ := createSlurm(t, rig.tokenRig, slurmCreateBody)
	pbs, _ := rig.create("ehpc-sz-2")
	unknown, _ := rig.create("ehpc-sz-9")
	revoked, _ := rig.create("ehpc-sz-1")
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+revoked+"/revoke", ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	for _, tc := range []struct{ name, uuid, reason string }{
		{"no source", noSource, "no_source"}, {"not Slurm", pbs, "not_slurm_cluster"},
		{"cluster not synced", unknown, "cluster_unknown"}, {"revoked", revoked, "revoked"},
	} {
		rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+tc.uuid+"/apply", `{"dry_run":false}`)
		got := decodeMap(t, rec.Body.Bytes())
		if rec.Code != http.StatusConflict || got["code"] != messages.ErrorSlurmApplyNotApplicable || !reflect.DeepEqual(got["params"], map[string]any{"reason": tc.reason}) {
			t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	if len(rig.drv.runs) != 0 {
		t.Fatalf("a refused apply reached the manager: %d runs", len(rig.drv.runs))
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/missing/apply", `{"dry_run":false}`); rec.Code != http.StatusNotFound {
		t.Fatalf("apply of a missing entry = %d", rec.Code)
	}
}

func TestApplySlurmClusterNeedsADriverThatRunsCommands(t *testing.T) {
	rig := newTokenRig(t)
	plain := &plainDriver{apiInventoryDriver{instances: map[string]provider.Instance{"i-1": ehpcNode("i-1", "gpu000", "ehpc-sz-1", "slurm", 1)}}}
	provider.Register(plain)
	box, _ := auth.NewSecretbox("plain-driver")
	svc := provider.NewService(rig.st, box)
	p, err := svc.Add(context.Background(), plain.Name(), "plain", provider.Credentials{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("plain-secret", time.Hour)
	admin, _ := rig.st.VerifyUser("admin", "gpuworks-test-password-2026")
	session, _ := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: admin.ID, Username: "admin", Role: "admin"})
	rig.h, rig.session = Router(Deps{Store: rig.st, Providers: svc, AuthSigner: signer}), session
	if rec := rig.do(session, http.MethodPost, fmt.Sprintf("/api/v1/providers/%d/sync", p.ID), ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	rec := rig.do(session, http.MethodPost, "/api/v1/slurm/clusters", fmt.Sprintf(`{"name":"e","server_url":"https://gw.example:8800","provider_id":%d,"provider_cluster_id":"ehpc-sz-1"}`, p.ID))
	entry := decodeMap(t, rec.Body.Bytes())
	if entry["can_apply"] != false {
		t.Fatalf("can_apply = %v", entry["can_apply"])
	}
	rec = rig.do(session, http.MethodPost, "/api/v1/slurm/clusters/"+entry["uuid"].(string)+"/apply", `{"dry_run":false}`)
	if got := decodeMap(t, rec.Body.Bytes()); rec.Code != http.StatusConflict || !reflect.DeepEqual(got["params"], map[string]any{"reason": "provider_unsupported"}) {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
}

func TestApplySlurmClusterErrorsAreCoded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		run    func(string) (provider.SlurmManagerRun, error)
		status int
		code   string
		params map[string]any
	}{
		{"permission", func(string) (provider.SlurmManagerRun, error) {
			return provider.SlurmManagerRun{}, &provider.SlurmApplyPermissionError{Permission: "ecs:RunCommand", Err: errors.New("403 Forbidden.RAM")}
		}, 424, messages.ErrorSlurmApplyPermissionMissing, map[string]any{"permission": "ecs:RunCommand"}},
		{"manager unknown", func(string) (provider.SlurmManagerRun, error) {
			return provider.SlurmManagerRun{}, provider.ErrSlurmApplyManagerUnknown
		}, 424, messages.ErrorSlurmApplyManagerUnknown, nil},
		{"cloud step", func(string) (provider.SlurmManagerRun, error) {
			return provider.SlurmManagerRun{}, &provider.SlurmApplyFailedError{Step: "assistant", Output: "Cloud Assistant is not online on i-mgr"}
		}, 424, messages.ErrorSlurmApplyFailed, map[string]any{"step": "assistant", "reason": "", "output": "Cloud Assistant is not online on i-mgr"}},
		{"setup precheck", okRun("ERROR: this script changes slurm.conf and must run as root (use sudo).\nGWKS_SLURM step=precheck status=failed reason=not_root\nGWKS_SLURM step=done status=failed\n"),
			424, messages.ErrorSlurmApplyFailed, map[string]any{"step": "setup", "reason": "not_root", "output": "ERROR: this script changes slurm.conf and must run as root (use sudo)."}},
		{"server unreachable", func(string) (provider.SlurmManagerRun, error) {
			return provider.SlurmManagerRun{ManagerInstance: "i-mgr", Output: "curl: (28) Connection timed out after 60001 milliseconds\nGWKS_APPLY fetch=failed rc=28\n"}, nil
		}, 424, messages.ErrorSlurmApplyFailed, map[string]any{"step": "setup", "reason": "server_unreachable", "output": "curl: (28) Connection timed out after 60001 milliseconds", "url": "https://gw.example:8800", "kind": "timeout"}},
		{"script died", func(string) (provider.SlurmManagerRun, error) {
			return provider.SlurmManagerRun{ManagerInstance: "i-mgr", Output: "sh: 1: scontrol: not found\n", Ended: "the command ended Failed with exit code 127"}, nil
		}, 424, messages.ErrorSlurmApplyFailed, map[string]any{"step": "run", "reason": "", "output": "the command ended Failed with exit code 127\nsh: 1: scontrol: not found"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApplyRig(t, tc.run)
			uuid, _ := rig.create("ehpc-sz-1")
			rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":false}`)
			got := decodeMap(t, rec.Body.Bytes())
			if rec.Code != tc.status || got["code"] != tc.code {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if tc.params != nil && !reflect.DeepEqual(got["params"], tc.params) {
				t.Fatalf("params = %v, want %v", got["params"], tc.params)
			}
		})
	}
}

func TestRemoveSlurmClusterRunsTheTwoUninstallSteps(t *testing.T) {
	step1 := "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=uninstall_agents status=ok interval=300\nGWKS_SLURM step=done status=ok\n"
	step2 := "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=remove status=ok path=/etc/slurm/slurm.conf backup=none\nGWKS_SLURM step=reconfigure status=ok\nGWKS_SLURM step=done status=ok\n"
	rig := newApplyRig(t, func(script string) (provider.SlurmManagerRun, error) {
		out := step2
		if strings.Contains(script, "mode='uninstall_agents'") {
			out = step1
		}
		return provider.SlurmManagerRun{ManagerInstance: "i-mgr", Output: out}, nil
	})
	uuid, _ := rig.create("ehpc-sz-1")
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/remove", `{"step":"uninstall_agents"}`)
	got := decodeMap(t, rec.Body.Bytes())
	delete(got, "cluster")
	if want := map[string]any{"step": "uninstall_agents", "status": "ok", "manager_instance": "i-mgr", "wait_seconds": float64(300), "next_step": "remove_check"}; rec.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("step 1 = %d %v", rec.Code, got)
	}
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/remove", `{"step":"remove_check"}`)
	got = decodeMap(t, rec.Body.Bytes())
	delete(got, "cluster")
	if want := map[string]any{"step": "remove_check", "status": "ok", "manager_instance": "i-mgr", "wait_seconds": float64(0)}; rec.Code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("step 2 = %d %v", rec.Code, got)
	}
	// Removing is not applying: no gres.conf line for any node.
	for _, run := range rig.drv.runs {
		if strings.Contains(run.script, "gpu000") {
			t.Fatalf("a removal step carries compute nodes:\n%s", run.script)
		}
	}
	// A revoked entry can still be removed.
	rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/revoke", "")
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/remove", `{"step":"remove_check"}`); rec.Code != http.StatusOK {
		t.Fatalf("remove of a revoked entry = %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"step":"reboot"}`, `{}`, `{"step":"remove_check"} x`, ``} {
		if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/remove", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("remove %q = %d %s", body, rec.Code, rec.Body)
		}
	}
	// Failures are coded like apply's.
	rig.drv.answer = okRun("sh: 1: curl: not found\n")
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/remove", `{"step":"uninstall_agents"}`)
	if got := decodeMap(t, rec.Body.Bytes()); rec.Code != 424 || got["code"] != messages.ErrorSlurmApplyFailed {
		t.Fatalf("failed step = %d %s", rec.Code, rec.Body)
	}
}

func TestSlurmApplyRoutesAreForAdministrators(t *testing.T) {
	rig := newApplyRig(t, okRun(setupDoneOutput))
	uuid, _ := rig.create("ehpc-sz-1")
	signer, _ := auth.NewTokenSigner("slurm-apply-user-secret", time.Hour)
	user, _ := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: 2, Username: "user", Role: "user"})
	rig.h = Router(Deps{Store: rig.st, Providers: nil, AuthSigner: signer})
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/slurm/clusters/" + uuid + "/apply", ""},
		{http.MethodPost, "/api/v1/slurm/clusters/" + uuid + "/remove", `{"step":"remove_check"}`},
	} {
		if rec := rig.do(user, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("user %s = %d, want 403", tc.path, rec.Code)
		}
		if rec := rig.do("", tc.method, tc.path, tc.body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d, want 401", tc.path, rec.Code)
		}
		if rec := rig.do(user, http.MethodGet, tc.path, ""); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusForbidden {
			t.Fatalf("GET %s = %d", tc.path, rec.Code)
		}
	}
}

// A node that enrolls through an entry made from a cloud cluster is the one
// its instance row names, whether the row was synced before or after.
func TestSlurmNodeEnrollmentLinksTheCloudInstance(t *testing.T) {
	rig := newApplyRig(t, okRun(""))
	uuid, entry := rig.create("ehpc-sz-1")
	_ = uuid
	token := strings.TrimSuffix(strings.TrimPrefix(entry["setup_command"].(string), "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token="), "' | sudo sh")
	token, _ = url.QueryUnescape(token)
	tok, _ := json.Marshal(token)
	enroll := func(name string) string {
		rec := rig.do("", http.MethodPost, "/api/v1/slurm/enroll", `{"token":`+string(tok)+`,"slurm_cluster_name":"demo","node_name":"`+name+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("enroll %s = %d %s", name, rec.Code, rec.Body)
		}
		return decodeMap(t, rec.Body.Bytes())["node_uuid"].(string)
	}
	nodeID := func(uuid string) uint {
		n, err := rig.st.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	linked := func(external string) uint {
		row, err := rig.st.ProviderInstanceByExternalID(rig.providerID, external)
		if err != nil || row == nil {
			t.Fatalf("row %s: %v", external, err)
		}
		return row.NodeID
	}
	n0 := nodeID(enroll("gpu000"))
	if linked("i-gpu000") != n0 || linked("i-gpu001") != 0 || linked("i-plain") != 0 {
		t.Fatalf("links after enrollment: gpu000 %d (node %d), gpu001 %d", linked("i-gpu000"), n0, linked("i-gpu001"))
	}
	// A sync keeps the link, and the account counts the node it joined though the instance list leaves a cluster's nodes out.
	if rec := rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/providers/%d/sync", rig.providerID), ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if linked("i-gpu000") != n0 {
		t.Fatal("a sync dropped the link")
	}
	if body := rig.do(rig.session, http.MethodGet, "/api/v1/providers", "").Body.String(); !strings.Contains(body, `"instance_count":1`) || !strings.Contains(body, `"node_count":1`) {
		t.Fatalf("the account counts its own instance and the node that joined through it: %s", body)
	}
	// A node that enrolled before its instance was synced is linked by the sync.
	if err := rig.st.DB().Model(&store.ProviderInstance{}).Where("external_id = ?", "i-gpu001").Update("name", "renamed").Error; err != nil {
		t.Fatal(err)
	}
	n1 := nodeID(enroll("gpu001"))
	if linked("i-gpu001") != 0 {
		t.Fatal("linked an instance with another name")
	}
	if rec := rig.do(rig.session, http.MethodPost, fmt.Sprintf("/api/v1/providers/%d/sync", rig.providerID), ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if linked("i-gpu001") != n1 {
		t.Fatalf("the sync did not link gpu001: %d, want %d", linked("i-gpu001"), n1)
	}
}

// checkOutput is what the manager prints for a check-only run: a plan with a
// gres line to add, a node to resume and a warning.
const checkOutput = "GWKS_APPLY gres=would_add node=gpu000 files=/dev/nvidia0\nGWKS_APPLY gres=present node=gpu001\n" +
	"GWKS_SLURM step=precheck status=ok cluster=demo\n" +
	"GWKS_SLURM step=check_script status=plan path=/opt/gpuworks-slurm/check.sh action=write\n" +
	"GWKS_SLURM step=token_file status=plan path=/opt/gpuworks-slurm/token\n" +
	"GWKS_SLURM step=chained status=plan program=/usr/sbin/nhc node_state=IDLE\n" +
	"GWKS_SLURM step=install_mode status=plan rearm=no\n" +
	"GWKS_SLURM step=shared status=no fstype=ext4\n" +
	"GWKS_SLURM step=distribute_plan nodes=gpu000,gpu001 files=check.sh,token,chained\n" +
	"GWKS_SLURM step=conf_comment line=7 key=HealthCheckProgram value=/usr/sbin/nhc\n" +
	"GWKS_SLURM step=conf_include status=warn files=/etc/slurm/extra.conf\n" +
	"GWKS_SLURM step=slurm_conf status=plan path=/etc/slurm/slurm.conf chained=/usr/sbin/nhc interval=300 node_state=ANY changed=yes\n" +
	"GWKS_SLURM step=configless value=yes\nGWKS_SLURM step=reconfigure status=plan\nGWKS_SLURM step=done status=partial mode=check\n" +
	"GWKS_APPLY resume=would node=gpu000\n"

// The apply says whether it checks or applies, never by omission; a check
// runs the manager script in check mode, answers the plan as literal wire
// JSON, leaves the entry as it was and spends no one-time code.
func TestApplySlurmClusterDryRunAnswersThePlanAndChangesNothing(t *testing.T) {
	rig := newApplyRig(t, okRun(checkOutput))
	uuid, _ := rig.create("ehpc-sz-1")
	path := "/api/v1/slurm/clusters/" + uuid + "/apply"
	for _, body := range []string{"", `{}`, `{"dry_run":null}`, `not json`, `{"dry_run":"yes"}`, `{"dry_run":true} {"dry_run":true}`} {
		rec := rig.do(rig.session, http.MethodPost, path, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q = %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := rig.do(rig.session, http.MethodPost, path, `{}`); !strings.Contains(rec.Body.String(), `"SLURM_CLUSTER_INVALID"`) || !strings.Contains(rec.Body.String(), `"dry_run"`) {
		t.Fatalf("a missing dry_run = %s", rec.Body)
	}
	if len(rig.drv.runs) != 0 {
		t.Fatal("a refused request reached the manager")
	}

	rec := rig.do(rig.session, http.MethodPost, path, `{"dry_run":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body)
	}
	got := decodeMap(t, rec.Body.Bytes())
	cluster := got["cluster"].(map[string]any)
	delete(got, "cluster")
	want := map[string]any{
		"dry_run": true, "status": "partial", "manager_instance": "i-mgr", "slurm_cluster": "demo",
		"gres": map[string]any{"would_add": []any{map[string]any{"node": "gpu000", "files": "/dev/nvidia0", "line": "NodeName=gpu000 Name=gpu File=/dev/nvidia0"}}, "present": []any{"gpu001"}, "no_gpu_gres": []any{}},
		"script": map[string]any{"check_script": "/opt/gpuworks-slurm/check.sh", "token_file": "/opt/gpuworks-slurm/token", "shared": false, "fstype": "ext4",
			"copy_to": []any{"gpu000", "gpu001"}, "copy_files": []any{"check.sh", "token", "chained"}},
		"conf": map[string]any{"path": "/etc/slurm/slurm.conf", "changed": true, "interval": float64(300), "node_state": "ANY",
			"block":           []any{"HealthCheckProgram=/opt/gpuworks-slurm/check.sh", "HealthCheckInterval=300", "HealthCheckNodeState=ANY"},
			"chained_program": "/usr/sbin/nhc", "chained_state": "IDLE",
			"commented": []any{map[string]any{"line": float64(7), "key": "HealthCheckProgram", "value": "/usr/sbin/nhc"}}, "includes": []any{"/etc/slurm/extra.conf"}},
		"configless": true, "rearm": false, "resume_nodes": []any{"gpu000"}, "warnings": []any{"conf_include"},
		"reach": map[string]any{"hosts": []any{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan\n got %v\nwant %v", got, want)
	}
	if cluster["uuid"] != uuid || cluster["node_count"] != float64(0) {
		t.Fatalf("the plan's entry = %v", cluster)
	}
	if len(rig.drv.runs) != 1 || !strings.Contains(rig.drv.runs[0].script, "mode='check'") || !strings.Contains(rig.drv.runs[0].script, "expect_gpu='gpu000\ngpu001'") {
		t.Fatalf("runs = %+v", rig.drv.runs)
	}
	// Nothing about the entry changed, and the one-time code the run was given is spent or dropped.
	list := decodeMap(t, rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "").Body.Bytes())["items"].([]any)
	if e := list[0].(map[string]any); e["state"] != "pending" || e["node_count"] != float64(0) {
		t.Fatalf("entry after a check = %v", e)
	}
	code := rig.drv.runs[0].script[strings.Index(rig.drv.runs[0].script, "?once=")+len("?once="):]
	code = code[:strings.Index(code, "'")]
	if rec := rig.do("", http.MethodGet, "/slurm/setup.sh?once="+code, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("the check's one-time code still works: %d", rec.Code)
	}
}

// A check is refused and fails where an apply is: the same codes.
func TestApplySlurmClusterDryRunFailsWhereTheApplyDoes(t *testing.T) {
	rig := newApplyRig(t, func(string) (provider.SlurmManagerRun, error) {
		return provider.SlurmManagerRun{}, &provider.SlurmApplyPermissionError{Permission: "ecs:RunCommand", Err: errors.New("denied")}
	})
	uuid, _ := rig.create("ehpc-sz-1")
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":true}`)
	got := decodeMap(t, rec.Body.Bytes())
	if rec.Code != statusProviderFailed || got["code"] != messages.ErrorSlurmApplyPermissionMissing || !reflect.DeepEqual(got["params"], map[string]any{"permission": "ecs:RunCommand"}) {
		t.Fatalf("permission = %d %s", rec.Code, rec.Body)
	}
	pbs, _ := rig.create("ehpc-sz-2")
	rec = rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+pbs+"/apply", `{"dry_run":true}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_slurm_cluster") {
		t.Fatalf("not applicable = %d %s", rec.Code, rec.Body)
	}
}

// The manager printed a setup's output, not a plan: a failed run, never a
// made-up plan.
func TestApplySlurmClusterDryRunWithoutAPlanIsAFailedRun(t *testing.T) {
	rig2 := newApplyRig(t, okRun(setupDoneOutput))
	uuid2, _ := rig2.create("ehpc-sz-1")
	rec := rig2.do(rig2.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid2+"/apply", `{"dry_run":true}`)
	if rec.Code != statusProviderFailed || !strings.Contains(rec.Body.String(), messages.ErrorSlurmApplyFailed) {
		t.Fatalf("no plan printed = %d %s", rec.Code, rec.Body)
	}
}
