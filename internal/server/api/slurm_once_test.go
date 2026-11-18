package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestSlurmOnceCodeServesOnce(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	codes := &slurmOnceCodes{now: func() time.Time { return now }}
	code, err := codes.issue(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) < 32 {
		t.Fatalf("code %q is too short to be a secret", code)
	}
	if id, ok := codes.take(code); !ok || id != 7 {
		t.Fatalf("first take = %d, %v", id, ok)
	}
	if _, ok := codes.take(code); ok {
		t.Fatal("a code was spent twice")
	}
	if _, ok := codes.take("not-a-code"); ok {
		t.Fatal("an unknown code was accepted")
	}
	if _, ok := codes.take(""); ok {
		t.Fatal("an empty code was accepted")
	}
}

func TestSlurmOnceCodeExpires(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	codes := &slurmOnceCodes{now: func() time.Time { return now }}
	late, _ := codes.issue(1)
	edge, _ := codes.issue(2)
	fresh, _ := codes.issue(3)
	now = now.Add(slurmOnceTTL - time.Second)
	if id, ok := codes.take(fresh); !ok || id != 3 {
		t.Fatalf("a code just inside its lifetime = %d, %v", id, ok)
	}
	now = now.Add(time.Second)
	if _, ok := codes.take(edge); ok {
		t.Fatal("a code was accepted at the end of its lifetime")
	}
	now = now.Add(time.Hour)
	if _, ok := codes.take(late); ok {
		t.Fatal("an expired code was accepted")
	}
	// Issuing sweeps what expired, so unspent codes do not pile up.
	dead, _ := codes.issue(4)
	now = now.Add(2 * slurmOnceTTL)
	if _, err := codes.issue(5); err != nil {
		t.Fatal(err)
	}
	if _, kept := codes.by[dead]; kept || len(codes.by) != 1 {
		t.Fatalf("expired codes kept: %d", len(codes.by))
	}
}

func TestSlurmOnceCodeConcurrentTakesHaveOneWinner(t *testing.T) {
	var codes slurmOnceCodes
	code, _ := codes.issue(9)
	var wg sync.WaitGroup
	wins := make(chan uint, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if id, ok := codes.take(code); ok {
				wins <- id
			}
		}()
	}
	wg.Wait()
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("%d fetches spent one code", n)
	}
}

func TestSlurmOnceCodeDiscardAndForget(t *testing.T) {
	var codes slurmOnceCodes
	a, _ := codes.issue(1)
	b, _ := codes.issue(1)
	c, _ := codes.issue(2)
	codes.discard(a)
	codes.forget(1)
	if _, ok := codes.take(a); ok {
		t.Fatal("a discarded code was accepted")
	}
	if _, ok := codes.take(b); ok {
		t.Fatal("a deleted entry's code was accepted")
	}
	if id, ok := codes.take(c); !ok || id != 2 {
		t.Fatal("another entry's code was dropped")
	}
}

var onceURL = regexp.MustCompile(`setup_url='https://gw\.example:8800(/slurm/setup\.sh\?once=[0-9a-f]+)'`)

// managerFetching is a manager node that does what the real one does with the
// script: it downloads the setup script from the one-time URL in it, and
// answers by what that download returned.
func managerFetching(rig *applyRig, fetched *sync.Map, output string) func(string) (provider.SlurmManagerRun, error) {
	return func(script string) (provider.SlurmManagerRun, error) {
		m := onceURL.FindStringSubmatch(script)
		if m == nil {
			return provider.SlurmManagerRun{}, fmt.Errorf("no one-time URL in the script:\n%s", script)
		}
		rec := rig.do("", http.MethodGet, m[1], "")
		fetched.Store(m[1], rec.Code)
		if rec.Code != http.StatusOK {
			return provider.SlurmManagerRun{ManagerInstance: "i-mgr", Output: "GWKS_APPLY fetch=failed\n"}, nil
		}
		return provider.SlurmManagerRun{ManagerInstance: "i-mgr", Output: output}, nil
	}
}

func TestApplyFetchesTheSetupScriptWithAOneTimeURLThatCarriesTheToken(t *testing.T) {
	rig := newApplyRig(t, okRun(""))
	var fetched sync.Map
	rig.drv.answer = managerFetching(rig, &fetched, setupDoneOutput)
	uuid, entry := rig.create("ehpc-sz-1")
	token := strings.TrimSuffix(strings.TrimPrefix(entry["setup_command"].(string), "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token="), "' | sudo sh")
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":false}`); rec.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	run := rig.drv.runs[0]
	if strings.Contains(run.script, token) {
		t.Fatal("the command sent to the cloud carries the entry's token")
	}
	path := onceURL.FindStringSubmatch(run.script)[1]
	if code, _ := fetched.Load(path); code != http.StatusOK {
		t.Fatalf("the manager's download = %v", code)
	}
	// The code the manager spent is spent: the download cannot be repeated,
	// by anyone who later reads the command out of the cloud's records.
	rec := rig.do("", http.MethodGet, path, "")
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), token) {
		t.Fatalf("a second download = %d %s", rec.Code, rec.Body)
	}
	// The script body is what carries the token, into the root-only file.
	var script string
	rig.drv.answer = func(s string) (provider.SlurmManagerRun, error) {
		rec := rig.do("", http.MethodGet, onceURL.FindStringSubmatch(s)[1], "")
		script = rec.Body.String()
		return provider.SlurmManagerRun{ManagerInstance: "i-mgr", Output: setupDoneOutput}, nil
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":false}`); rec.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(script, "TOKEN='"+token+"'") || !strings.Contains(script, `umask 077`) {
		t.Fatalf("the downloaded script does not carry the token for the token file:\n%.600s", script)
	}
	if strings.Contains(script, "/slurm/token-status?token="+token) {
		t.Fatal("the downloaded script carries the token in a URL")
	}
}

func TestApplyAgainIssuesAnotherCode(t *testing.T) {
	rig := newApplyRig(t, okRun(""))
	var fetched sync.Map
	rig.drv.answer = managerFetching(rig, &fetched, setupDoneOutput)
	uuid, _ := rig.create("ehpc-sz-1")
	for i := 0; i < 2; i++ {
		if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":false}`); rec.Code != http.StatusOK {
			t.Fatalf("apply %d = %d %s", i, rec.Code, rec.Body)
		}
	}
	seen := map[string]bool{}
	for _, run := range rig.drv.runs {
		seen[onceURL.FindStringSubmatch(run.script)[1]] = true
	}
	if len(seen) != 2 {
		t.Fatalf("two applies used %d one-time URLs", len(seen))
	}
}

func TestConcurrentAppliesEachFetchWithTheirOwnCode(t *testing.T) {
	rig := newApplyRig(t, okRun(""))
	var fetched sync.Map
	rig.drv.answer = managerFetching(rig, &fetched, setupDoneOutput)
	uuid, _ := rig.create("ehpc-sz-1")
	const n = 8
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":false}`).Code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("a concurrent apply answered %d", code)
		}
	}
	count := 0
	fetched.Range(func(_, v any) bool {
		count++
		if v != http.StatusOK {
			t.Errorf("a download answered %v", v)
		}
		return true
	})
	if count != n {
		t.Fatalf("%d distinct one-time URLs for %d applies", count, n)
	}
}

func TestAnUnspentCodeDiesWithTheApply(t *testing.T) {
	// The cloud refuses the command before the manager downloads anything.
	rig := newApplyRig(t, func(string) (provider.SlurmManagerRun, error) {
		return provider.SlurmManagerRun{}, &provider.SlurmApplyFailedError{Step: "assistant", Output: "not online"}
	})
	uuid, _ := rig.create("ehpc-sz-1")
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/apply", `{"dry_run":false}`); rec.Code != 424 {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body)
	}
	if len(rig.drv.runs) != 1 {
		t.Fatalf("runs = %d", len(rig.drv.runs))
	}
	path := onceURL.FindStringSubmatch(rig.drv.runs[0].script)[1]
	if rec := rig.do("", http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("the code of a failed apply still serves the script: %d", rec.Code)
	}
}

func TestRemoveStepsFetchWithAOneTimeURLToo(t *testing.T) {
	rig := newApplyRig(t, okRun(""))
	var fetched sync.Map
	step1 := "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=uninstall_agents status=ok interval=300\nGWKS_SLURM step=done status=ok\n"
	step2 := "GWKS_SLURM step=precheck status=ok cluster=demo\nGWKS_SLURM step=remove status=ok path=/etc/slurm/slurm.conf backup=none\nGWKS_SLURM step=reconfigure status=ok\nGWKS_SLURM step=done status=ok\n"
	fetch1, fetch2 := managerFetching(rig, &fetched, step1), managerFetching(rig, &fetched, step2)
	rig.drv.answer = func(script string) (provider.SlurmManagerRun, error) {
		if strings.Contains(script, "mode='uninstall_agents'") {
			return fetch1(script)
		}
		return fetch2(script)
	}
	uuid, entry := rig.create("ehpc-sz-1")
	token := strings.TrimSuffix(strings.TrimPrefix(entry["setup_command"].(string), "curl -fsSL 'https://gw.example:8800/slurm/setup.sh?token="), "' | sudo sh")
	for _, step := range []string{"uninstall_agents", "remove_check"} {
		if rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/remove", `{"step":"`+step+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", step, rec.Code, rec.Body)
		}
	}
	for _, run := range rig.drv.runs {
		if strings.Contains(run.script, token) {
			t.Fatalf("a removal step's command carries the token:\n%s", run.script)
		}
	}
	if len(rig.drv.runs) != 2 {
		t.Fatalf("runs = %d", len(rig.drv.runs))
	}
}

func TestOneTimeCodesServeOnlyTheSetupScript(t *testing.T) {
	rig := newApplyRig(t, okRun(""))
	rig.create("ehpc-sz-1")
	for _, path := range []string{"/slurm/setup.sh?once=0123", "/slurm/check.sh?once=0123", "/slurm/setup.sh?once=", "/slurm/setup.sh"} {
		if rec := rig.do("", http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}
