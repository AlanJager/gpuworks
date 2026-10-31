package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

const capabilitiesPDPlacement = `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"a"}]},"decode":{"tp":1,"units":[{"node_uuid":"b"}]}}}}`

// deploymentRawFields reads the named fields of GET /deployments/{id} as the
// server wrote them, key order and all.
func deploymentRawFields(t *testing.T, rig *tokenRig, dep *store.Deployment, names ...string) map[string]string {
	t.Helper()
	rec := rig.do(rig.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", dep.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET deployment = %d %s", rec.Code, rec.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, name := range names {
		var compact bytes.Buffer
		if field, ok := raw[name]; ok {
			if err := json.Compact(&compact, field); err != nil {
				t.Fatal(err)
			}
			out[name] = compact.String()
		}
	}
	return out
}

// A deployment's read says what a client may offer it, as the server
// decides: whether it may hold a yield policy other than never, what its
// schedule may order with the replica bounds of each role, and how long a
// waiting wake lasts. A client renders these and keeps no copy of the rules.
func TestTheDeploymentSaysWhatMayBeOffered(t *testing.T) {
	rig := newYieldPolicyRig(t)
	create := func(dep *store.Deployment) *store.Deployment {
		dep.State = store.DeployActive
		dep.ServedName = dep.Name
		created, err := rig.st.CreateDeployment(dep)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	pdManual := create(&store.Deployment{Name: "pd-manual", Engine: "vllm", Sleepable: true, Replicas: 1, Placement: capabilitiesPDPlacement})
	pdAuto := create(&store.Deployment{Name: "pd-auto", Engine: "vllm", Sleepable: true, Replicas: 1, Placement: capabilitiesPDPlacement, UnitsAuto: true})
	const single = `[{"minimum":1,"maximum":32,"ok":true}]`
	const manual = `"SCHEDULE_REPLICAS_MANUAL_PLACEMENT"`
	for _, tc := range []struct {
		name      string
		dep       *store.Deployment
		eligible  string
		schedule  string
		waitLimit string
	}{
		{"sleepable single-node", rig.sleepy, `{"ok":true}`,
			`{"sleep":{"ok":true},"wake":{"ok":true},"replicas":` + single + `}`, `1800`},
		{"not sleepable", rig.plain, `{"ok":false,"reason":"SLEEP_NOT_ENABLED"}`,
			`{"sleep":{"ok":false,"reason":"SLEEP_NOT_ENABLED"},"wake":{"ok":false,"reason":"SLEEP_NOT_ENABLED"},"replicas":` + single + `}`, `1800`},
		{"multi-node placed by hand", rig.multi, `{"ok":false,"reason":"YIELD_POLICY_SHAPE"}`,
			`{"sleep":{"ok":true},"wake":{"ok":true},"replicas":[{"minimum":1,"maximum":32,"ok":false,"reason":` + manual + `}]}`, `1800`},
		{"PD placed by hand", pdManual, `{"ok":false,"reason":"YIELD_POLICY_SHAPE"}`,
			`{"sleep":{"ok":true},"wake":{"ok":true},"replicas":[{"role":"decode","minimum":1,"maximum":32,"ok":false,"reason":` + manual + `},{"role":"prefill","minimum":0,"maximum":32,"ok":false,"reason":` + manual + `}]}`, `1800`},
		{"PD placed by the platform", pdAuto, `{"ok":false,"reason":"YIELD_POLICY_SHAPE"}`,
			`{"sleep":{"ok":true},"wake":{"ok":true},"replicas":[{"role":"decode","minimum":1,"maximum":32,"ok":true},{"role":"prefill","minimum":0,"maximum":32,"ok":true}]}`, `1800`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deploymentRawFields(t, rig.tokenRig, tc.dep, "yield_policy_eligible", "schedule_capabilities", "wake_wait_limit_sec")
			if got["yield_policy_eligible"] != tc.eligible {
				t.Errorf("yield_policy_eligible = %s, want %s", got["yield_policy_eligible"], tc.eligible)
			}
			if got["schedule_capabilities"] != tc.schedule {
				t.Errorf("schedule_capabilities = %s\nwant %s", got["schedule_capabilities"], tc.schedule)
			}
			if got["wake_wait_limit_sec"] != tc.waitLimit {
				t.Errorf("wake_wait_limit_sec = %s, want %s", got["wake_wait_limit_sec"], tc.waitLimit)
			}
		})
	}
}

// What the read says is what the route does: a deployment the read marks
// not eligible is refused a policy other than never with the read's reason,
// and may still go back to never; an eligible one is accepted.
func TestTheYieldPolicyRouteAgreesWithTheRead(t *testing.T) {
	rig := newYieldPolicyRig(t)
	for _, dep := range []*store.Deployment{rig.plain, rig.sleepy, rig.multi} {
		read := deploymentRawFields(t, rig.tokenRig, dep, "yield_policy_eligible")["yield_policy_eligible"]
		var eligible struct {
			OK     bool   `json:"ok"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(read), &eligible); err != nil {
			t.Fatal(err)
		}
		rec := rig.do(rig.session, http.MethodPut, schedPath(dep, "yield-policy"), `{"policy":"idle"}`)
		switch {
		case eligible.OK && rec.Code != http.StatusOK:
			t.Errorf("%s: read eligible, PUT idle = %d %s", dep.Name, rec.Code, rec.Body)
		case !eligible.OK && (rec.Code != http.StatusBadRequest || decodeBody(t, rec)["code"] != eligible.Reason):
			t.Errorf("%s: read %s, PUT idle = %d %s", dep.Name, read, rec.Code, rec.Body)
		}
		if rec := rig.do(rig.session, http.MethodPut, schedPath(dep, "yield-policy"), `{"policy":"never"}`); rec.Code != http.StatusOK {
			t.Errorf("%s: PUT never = %d %s", dep.Name, rec.Code, rec.Body)
		}
	}
}
