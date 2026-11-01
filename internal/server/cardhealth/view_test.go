package cardhealth

import (
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestBuildView(t *testing.T) {
	received := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	snap := NodeSnapshot{ReceivedAt: received, Online: true, Reports: true, Cards: []CardSnapshot{
		{Ident: "a", Index: 0, Checks: []proto.CardCheck{
			{ID: accelerator.CheckRuntime, State: accelerator.CheckPass, AgeS: 300, MS: 800},
			{ID: accelerator.CheckVendorHealth, State: accelerator.CheckPass},
			{ID: accelerator.CheckNVLink, State: accelerator.CheckNotApplicable},
		}},
		{Ident: "b", Index: 1, Checks: []proto.CardCheck{
			{ID: accelerator.CheckVendorHealth, State: accelerator.CheckFail, Reason: accelerator.HealthMemoryUncorrectable},
			{ID: accelerator.CheckRuntime, State: accelerator.CheckPending},
		}},
	}}
	v := BuildView(snap, received.Add(30*time.Second))
	if strings.Join(v.Checks, ",") != "vendor_health,runtime" || v.Stale {
		t.Fatalf("columns %v stale %v", v.Checks, v.Stale)
	}
	runtime := v.Cards[0].Cells[1]
	if !runtime.CheckedAt.Equal(received.Add(-5*time.Minute)) || runtime.DurationMS != 800 {
		t.Fatalf("runtime cell = %+v", runtime)
	}
	if pending := v.Cards[1].Cells[1]; !pending.CheckedAt.IsZero() {
		t.Fatalf("a pending check has a time: %+v", pending)
	}
	if Worst(v.Cards[0].Cells) != accelerator.CheckPass || Worst(v.Cards[1].Cells) != accelerator.CheckFail {
		t.Fatal("worst")
	}
	if late := BuildView(snap, received.Add(2*accelerator.ReportPeriod+staleGrace+time.Second)); !late.Stale || !late.Cards[0].Cells[0].Stale {
		t.Fatal("a late report is not stale")
	}
	snap.Online = false
	if off := BuildView(snap, received); !off.Stale {
		t.Fatal("an offline node is not stale")
	}
}
