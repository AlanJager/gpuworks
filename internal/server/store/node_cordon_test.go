package store

import (
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDecideScheduling(t *testing.T) {
	const lic SchedulingSource = "maint"
	const other SchedulingSource = "freeze"
	for _, tc := range []struct {
		name   string
		was    NodeScheduling
		by     SchedulingSource
		reason string
		known  bool
		hold   SchedulingSource
		want   NodeScheduling
		wantBy SchedulingSource
		change bool
	}{
		{"schedulable with a reason is cordoned by the platform", NodeSchedulable, SchedulingBySelf, CordonCardNeedsHostReboot, true, "", NodeCordoned, SchedulingByPlatform, true},
		{"the platform's cordon without a reason is lifted", NodeCordoned, SchedulingByPlatform, "", true, "", NodeSchedulable, SchedulingBySelf, true},
		{"the platform's cordon is kept while the runtime cannot say", NodeCordoned, SchedulingByPlatform, "", false, "", NodeCordoned, SchedulingByPlatform, false},
		{"the platform's cordon with a reason stays", NodeCordoned, SchedulingByPlatform, CordonRuntimeGPUBlocked, true, "", NodeCordoned, SchedulingByPlatform, false},
		{"an operator's cordon is never lifted", NodeCordoned, SchedulingByOperator, "", true, "", NodeCordoned, SchedulingByOperator, false},
		{"a cordon of unknown author is never lifted", NodeCordoned, SchedulingBySelf, "", true, "", NodeCordoned, SchedulingBySelf, false},
		{"a drain is never touched", NodeDraining, SchedulingByOperator, CordonCardNeedsHostReboot, true, "", NodeDraining, SchedulingByOperator, false},
		{"schedulable without a reason stays", NodeSchedulable, SchedulingBySelf, "", true, "", NodeSchedulable, SchedulingBySelf, false},

		{"a held node is cordoned under the holder", NodeSchedulable, SchedulingBySelf, "", true, lic, NodeCordoned, lic, true},
		{"a hold is applied when the runtime cannot say", NodeSchedulable, SchedulingBySelf, "", false, lic, NodeCordoned, lic, true},
		{"a hold outranks a platform reason", NodeSchedulable, SchedulingBySelf, CordonKubeUnschedulable, true, lic, NodeCordoned, lic, true},
		{"a hold takes over the platform's cordon", NodeCordoned, SchedulingByPlatform, CordonRuntimeGPUBlocked, true, lic, NodeCordoned, lic, true},
		{"a platform lift does not lift a hold", NodeCordoned, lic, "", true, lic, NodeCordoned, lic, false},
		{"an ended hold falls back to the platform's reason", NodeCordoned, lic, CordonCardNeedsHostReboot, true, "", NodeCordoned, SchedulingByPlatform, true},
		{"an ended hold falls back to kube_unschedulable", NodeCordoned, lic, CordonKubeUnschedulable, false, "", NodeCordoned, SchedulingByPlatform, true},
		{"an ended hold without a reason is lifted", NodeCordoned, lic, "", true, "", NodeSchedulable, SchedulingBySelf, true},
		{"an ended hold is lifted when the runtime cannot say", NodeCordoned, lic, "", false, "", NodeSchedulable, SchedulingBySelf, true},
		{"another holder takes the node over", NodeCordoned, lic, "", true, other, NodeCordoned, other, true},
		{"a hold never touches an operator's cordon", NodeCordoned, SchedulingByOperator, "", true, lic, NodeCordoned, SchedulingByOperator, false},
		{"a hold never touches a drain", NodeDraining, SchedulingByOperator, "", true, lic, NodeDraining, SchedulingByOperator, false},
		{"a hold never touches a cordon of unknown author", NodeCordoned, SchedulingBySelf, "", true, lic, NodeCordoned, SchedulingBySelf, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := decideScheduling(tc.was, tc.by, tc.reason, tc.known, tc.hold)
			if d.scheduling != tc.want || d.source != tc.wantBy || d.changed != tc.change {
				t.Fatalf("decideScheduling = %+v, want %q by %q changed=%v", d, tc.want, tc.wantBy, tc.change)
			}
			if d.scheduling == NodeDraining && tc.was != NodeDraining {
				t.Fatal("a decision produced a drain")
			}
		})
	}
}

func TestPlatformCordonReason(t *testing.T) {
	healthy := proto.GPUInfo{Index: 0, HealthCode: ""}
	reboot := proto.GPUInfo{Index: 1, HealthCode: accelerator.HealthRebootRequired}
	cycle := proto.GPUInfo{Index: 2, HealthCode: accelerator.HealthPowerCycleRequired}
	reset := proto.GPUInfo{Index: 3, HealthCode: accelerator.HealthResetRequired}
	lent := proto.GPUInfo{Index: 4, HealthCode: accelerator.HealthRebootRequired, Ownership: accelerator.CardPassedThrough}
	for _, tc := range []struct {
		blocked string
		cards   []proto.GPUInfo
		want    string
	}{
		{"", []proto.GPUInfo{healthy}, ""},
		{accelerator.RuntimeGPUBlockedPluginMissing, []proto.GPUInfo{healthy}, CordonRuntimeGPUBlocked},
		{"", []proto.GPUInfo{healthy, reboot}, CordonCardNeedsHostReboot},
		{"", []proto.GPUInfo{cycle}, CordonCardNeedsHostReboot},
		// A card reset is done on the card; the rest of the node can work.
		{"", []proto.GPUInfo{reset}, ""},
		// A card lent to a virtual machine is not this node's to judge.
		{"", []proto.GPUInfo{lent}, ""},
	} {
		if got := PlatformCordonReason(tc.blocked, true, "", tc.cards); got != CordonKubeUnschedulable {
			t.Errorf("PlatformCordonReason(%q, kube cordon, %+v) = %q, want %q", tc.blocked, tc.cards, got, CordonKubeUnschedulable)
		}
		if got := PlatformCordonReason(tc.blocked, false, "", tc.cards); got != tc.want {
			t.Errorf("PlatformCordonReason(%q, %+v) = %q, want %q", tc.blocked, tc.cards, got, tc.want)
		}
		// Slurm not running jobs on the node is a reason of its own, ranked
		// below Kubernetes' cordon and above the others.
		if got := PlatformCordonReason(tc.blocked, false, proto.SlurmNodeDrained, tc.cards); got != CordonSlurmNodeUnavailable {
			t.Errorf("PlatformCordonReason(%q, slurm drained, %+v) = %q, want %q", tc.blocked, tc.cards, got, CordonSlurmNodeUnavailable)
		}
		if got := PlatformCordonReason(tc.blocked, true, proto.SlurmNodeDrained, tc.cards); got != CordonKubeUnschedulable {
			t.Errorf("PlatformCordonReason(%q, kube cordon and slurm drained, %+v) = %q, want %q", tc.blocked, tc.cards, got, CordonKubeUnschedulable)
		}
	}
}

// A card whose vendor says only a host reboot recovers it takes the whole
// node out of scheduling: the other cards sit behind the same driver. An
// operator may turn scheduling back on to try a deployment by hand; the next
// report cordons it again while the card still says so; once it does not,
// the platform lifts its own cordon.
func TestACardNeedingAHostRebootCordonsTheNode(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-reboot", HashToken("t"), "reboot-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	sys := proto.SysInfo{RuntimeDriver: "docker"}
	report := func(code string) Node {
		t.Helper()
		cards := []proto.GPUInfo{
			{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true},
			{Index: 1, UUID: "GPU-b", Vendor: "nvidia", PCIeBDF: "0000:40:00.0", MemoryTotalBytes: 24 << 30,
				HealthCode: code, HealthStatus: accelerator.HealthLevel(code), Healthy: code == ""},
		}
		if err := s.UpdateNodeStatus("uuid-reboot", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: sys}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeByUUID("uuid-reboot")
		if err != nil {
			t.Fatal(err)
		}
		return *node
	}
	if node := report(accelerator.HealthRebootRequired); node.Scheduling != NodeCordoned || node.SchedulingBy != SchedulingByPlatform {
		t.Fatalf("scheduling = %q by %q, want cordoned by the platform", node.Scheduling, node.SchedulingBy)
	}
	if _, err := s.SetNodeScheduling("uuid-reboot", NodeSchedulable); err != nil {
		t.Fatal(err)
	}
	if node, _ := s.NodeByUUID("uuid-reboot"); node.Scheduling != NodeSchedulable || node.SchedulingBy != SchedulingBySelf {
		t.Fatalf("after the operator turned it on: %q by %q", node.Scheduling, node.SchedulingBy)
	}
	if node := report(accelerator.HealthRebootRequired); node.Scheduling != NodeCordoned {
		t.Fatalf("scheduling = %q, want it cordoned again while the card still needs a reboot", node.Scheduling)
	}
	if node := report(""); node.Scheduling != NodeSchedulable || node.SchedulingBy != SchedulingBySelf {
		t.Fatalf("after the reboot: %q by %q, want the platform's cordon lifted", node.Scheduling, node.SchedulingBy)
	}
}

// An operator's cordon on top of the platform's survives the platform's
// reason going away: the platform lifts only what it set itself.
func TestTheOperatorsCordonOutlivesThePlatformsReason(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-both", HashToken("t"), "both-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	cards := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}}
	report := func(blocked string) Node {
		t.Helper()
		if err := s.UpdateNodeStatus("uuid-both", proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards,
			Sys: proto.SysInfo{RuntimeDriver: "docker", RuntimeGPUBlocked: blocked}}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeByUUID("uuid-both")
		if err != nil {
			t.Fatal(err)
		}
		return *node
	}
	if node := report(accelerator.RuntimeGPUBlockedPluginMissing); node.SchedulingBy != SchedulingByPlatform {
		t.Fatalf("by = %q", node.SchedulingBy)
	}
	// The operator cordons it too, for maintenance of their own.
	if _, err := s.SetNodeScheduling("uuid-both", NodeCordoned); err != nil {
		t.Fatal(err)
	}
	if node := report(""); node.Scheduling != NodeCordoned || node.SchedulingBy != SchedulingByOperator {
		t.Fatalf("after the plugin was installed: %q by %q, want the operator's cordon left", node.Scheduling, node.SchedulingBy)
	}
}

// The column arrives empty on existing rows; applying it again changes
// nothing; schema and marker roll back together.
func TestNodeSchedulingSourceMigration(t *testing.T) {
	db := nodesOnlyDB(t, "scheduling TEXT")
	m := migrationByName(t, "node_scheduling_source")
	if err := db.Exec(`INSERT INTO nodes (uuid, scheduling) VALUES ('n1', 'cordoned')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn("nodes", "scheduling_by") {
		t.Fatal("scheduling_by is missing after the migration")
	}
	var by string
	if err := db.Raw(`SELECT scheduling_by FROM nodes WHERE uuid = 'n1'`).Scan(&by).Error; err != nil || by != "" {
		t.Fatalf("existing cordon by = %q, %v; want empty (author unknown)", by, err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("second application: %v", err)
	}

	t.Run("rollback", func(t *testing.T) {
		failing := nodesOnlyDB(t, "scheduling TEXT")
		if err := failing.Exec(fmt.Sprintf(`CREATE TRIGGER fail_marker BEFORE INSERT ON schema_migrations WHEN NEW.version=%d BEGIN SELECT RAISE(ABORT, 'injected'); END`, m.Version)).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateDB(failing, []migration{m}); err == nil {
			t.Fatal("injected marker failure did not fail the migration")
		}
		if failing.Migrator().HasColumn("nodes", "scheduling_by") {
			t.Fatal("the column survived a failed migration")
		}
	})
}

// A Slurm node whose controller will not start jobs on it is cordoned by the
// platform (every placeholder job would pend), shows Slurm's own words, and
// comes back when Slurm runs jobs there again; an operator's cordon is never
// lifted by it.
func TestASlurmNodeSlurmWillNotRunJobsOnIsCordoned(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-slurm", HashToken("t"), "slurm-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	const reason = "gres/gpu count reported lower than configured (0 < 2)"
	report := func(code, why string) Node {
		t.Helper()
		sys := proto.SysInfo{RuntimeDriver: "docker", SlurmNode: "gpu-1", SlurmUnavailable: code, SlurmReason: why}
		if err := s.UpdateNodeStatus("uuid-slurm", proto.NodeStatus{GPUProbeOK: &ok, Sys: sys}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeByUUID("uuid-slurm")
		if err != nil {
			t.Fatal(err)
		}
		return *node
	}
	if n := report("", ""); n.Scheduling != NodeSchedulable || n.SlurmNode != "gpu-1" || n.SlurmUnavailable != "" {
		t.Fatalf("a node Slurm runs jobs on = %q node=%q code=%q", n.Scheduling, n.SlurmNode, n.SlurmUnavailable)
	}
	n := report(proto.SlurmNodeGRESMissing, reason)
	if n.Scheduling != NodeCordoned || n.SchedulingBy != SchedulingByPlatform || n.CordonReason() != CordonSlurmNodeUnavailable ||
		n.SlurmUnavailable != proto.SlurmNodeGRESMissing || n.SlurmReason != reason {
		t.Fatalf("a drained Slurm node = %q by %q for %q, code %q reason %q", n.Scheduling, n.SchedulingBy, n.CordonReason(), n.SlurmUnavailable, n.SlurmReason)
	}
	if n := report(proto.SlurmNodeDrained, "maintenance"); n.Scheduling != NodeCordoned || n.SlurmUnavailable != proto.SlurmNodeDrained || n.SlurmReason != "maintenance" {
		t.Fatalf("the reason changed under the cordon: %q %q %q", n.Scheduling, n.SlurmUnavailable, n.SlurmReason)
	}
	n = report("", "")
	if n.Scheduling != NodeSchedulable || n.SchedulingBy != SchedulingBySelf || n.CordonReason() != "" || n.SlurmUnavailable != "" || n.SlurmReason != "" {
		t.Fatalf("after resume = %q by %q for %q code %q reason %q; want the platform's cordon lifted and Slurm's reason forgotten",
			n.Scheduling, n.SchedulingBy, n.CordonReason(), n.SlurmUnavailable, n.SlurmReason)
	}

	// An operator's own cordon is neither replaced nor lifted.
	if _, err := s.SetNodeScheduling("uuid-slurm", NodeCordoned); err != nil {
		t.Fatal(err)
	}
	if n := report(proto.SlurmNodeDown, "Not responding"); n.Scheduling != NodeCordoned || n.SchedulingBy != SchedulingByOperator || n.CordonReason() != "" {
		t.Fatalf("operator cordon under a Slurm reason = %q by %q for %q", n.Scheduling, n.SchedulingBy, n.CordonReason())
	}
	if n := report("", ""); n.Scheduling != NodeCordoned || n.SchedulingBy != SchedulingByOperator {
		t.Fatalf("operator cordon after resume = %q by %q", n.Scheduling, n.SchedulingBy)
	}
}

// What Slurm says is kept within its columns, so a long Reason cannot fail
// the status write.
func TestASlurmReasonIsTruncatedToItsColumn(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-long", HashToken("t"), "long-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	sys := proto.SysInfo{RuntimeDriver: "docker", SlurmNode: "gpu-1", SlurmUnavailable: proto.SlurmNodeDrained, SlurmReason: strings.Repeat("x", 1000)}
	if err := s.UpdateNodeStatus("uuid-long", proto.NodeStatus{GPUProbeOK: &ok, Sys: sys}); err != nil {
		t.Fatal(err)
	}
	n, err := s.NodeByUUID("uuid-long")
	if err != nil || len(n.SlurmReason) != 256 {
		t.Fatalf("reason length = %d (%v), want 256", len(n.SlurmReason), err)
	}
}
