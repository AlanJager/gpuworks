package api

import "testing"

func TestProbePDWake(t *testing.T) {
	f := newPDStateFixture(t)
	f.sleepRole(t, "prefill", 1)
	t.Logf("after sleep: %s", f.dto(t))
	f.reported(t, "prefill-0", 1)
	t.Logf("after report: %s", f.dto(t))
	f.sleepRole(t, "prefill", 0)
	for _, u := range []string{"prefill-0", "prefill-1", "decode-0"} {
		inst, _ := f.store.InstanceByUUID(u)
		t.Logf("%s intent=%d level=%d spec=%s", u, inst.SleepIntent(), inst.SleepLevel, inst.DesiredStateJSON)
	}
	t.Logf("after wake: %s", f.dto(t))
	f.reported(t, "prefill-0", 0)
	t.Logf("after report0: %s", f.dto(t))
}
