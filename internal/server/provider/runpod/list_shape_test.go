package runpod

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// Sync removes rows for pods the listing omits, so an unrecognised body must
// be an error rather than "no pods".
func TestListInstancesEnvelopesAndUnknownShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantIDs []string
		wantErr bool
	}{
		{name: "bare empty array", body: `[]`, wantIDs: []string{}},
		{name: "pods envelope", body: `{"pods":[{"id":"p1"}]}`, wantIDs: []string{"p1"}},
		{name: "data envelope", body: `{"data":[{"id":"p2"}]}`, wantIDs: []string{"p2"}},
		{name: "empty pods envelope", body: `{"pods":[]}`, wantIDs: []string{}},
		{name: "unknown envelope", body: `{"items":[{"id":"p3"}]}`, wantErr: true},
		{name: "error object with 200", body: `{"error":"rate limited"}`, wantErr: true},
		{name: "null pods", body: `{"pods":null}`, wantErr: true},
		{name: "null body", body: `null`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			insts, err := d.ListInstances(context.Background(), provider.Credentials{APIKey: "k"})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ListInstances(%s) = %+v, want error", tc.body, insts)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListInstances(%s): %v", tc.body, err)
			}
			ids := make([]string, 0, len(insts))
			for _, inst := range insts {
				ids = append(ids, inst.ExternalID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

// RunPod's pod list (GET /v2/pods, observed 2026-10-26) reports a running pod
// with "runtime": null and "ssh": {"direct": null, ...}: only the single-pod
// read carries the direct SSH endpoint. A recovery that read the endpoint
// from the list never saw a renumbered SSH port.
func TestListedRunningPodHasNoDirectSSHButItsDetailDoes(t *testing.T) {
	const listed = `{"pagination":{},"pods":[{"id":"pod-1","name":"gpu","status":"RUNNING","ports":["22/tcp","18800/tcp","18899/tcp"],"runtime":null,` +
		`"ssh":{"direct":null,"proxy":{"command":"ssh pod-1-64410f3e@ssh.runpod.io","host":"ssh.runpod.io","port":22,"username":"pod-1-64410f3e"}}}]}`
	const detail = `{"id":"pod-1","name":"gpu","status":"RUNNING","ports":["22/tcp","18800/tcp","18899/tcp"],` +
		`"runtime":{"uptime":14,"ports":[{"ip":"213.173.105.17","private":18800,"public":19816,"type":"tcp"},{"ip":"213.173.105.17","private":22,"public":19815,"type":"tcp"}]},` +
		`"ssh":{"direct":{"command":"ssh root@213.173.105.17 -p 19815","host":"213.173.105.17","port":19815,"username":"root"},` +
		`"proxy":{"command":"ssh pod-1-64410f3e@ssh.runpod.io","host":"ssh.runpod.io","port":22,"username":"pod-1-64410f3e"}}}`
	d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pods/pod-1") {
			_, _ = w.Write([]byte(detail))
			return
		}
		_, _ = w.Write([]byte(listed))
	})
	creds := provider.Credentials{APIKey: "k"}
	insts, err := d.ListInstances(context.Background(), creds)
	if err != nil || len(insts) != 1 {
		t.Fatalf("ListInstances = %+v, %v", insts, err)
	}
	if candidates, err := provider.SSHInstallCandidates(insts[0], "auto"); err == nil {
		t.Fatalf("the listed pod offers SSH %+v; its list carries none", candidates)
	}
	described, err := d.GetInstance(context.Background(), creds, "pod-1")
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := provider.SSHInstallCandidates(described, "auto")
	if err != nil || len(candidates) != 1 || candidates[0].Host != "213.173.105.17" || candidates[0].Port != 19815 || candidates[0].User != "root" {
		t.Fatalf("described pod SSH = %+v, %v", candidates, err)
	}
}
