package planning

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const schedulePDPlacement = `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"a"}]},"decode":{"tp":1,"units":[{"node_uuid":"b"}]}}}}`

func scheduleCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var product *messages.ProductError
	if !errors.As(err, &product) {
		t.Fatalf("err = %v, want a product error", err)
	}
	return product.Code()
}

// The bounds of a scheduled replica count: 1..MaxReplicas for any deployment
// but PD; a PD role 1..MaxUnitsPerRole, or from 0 where the mode lets the
// role be empty.
func TestReplicaBounds(t *testing.T) {
	plain := &store.Deployment{}
	pd := &store.Deployment{Placement: schedulePDPlacement}
	for _, tc := range []struct {
		name            string
		dep             *store.Deployment
		role            string
		lowest, highest int
	}{
		{"single-node", plain, "", 1, MaxReplicas},
		{"PD prefill may be empty", pd, "prefill", 0, pdmode.MaxUnitsPerRole},
		{"PD decode may not", pd, "decode", 1, pdmode.MaxUnitsPerRole},
	} {
		if lowest, highest := ReplicaBounds(tc.dep, tc.role); lowest != tc.lowest || highest != tc.highest {
			t.Fatalf("%s: bounds %d..%d, want %d..%d", tc.name, lowest, highest, tc.lowest, tc.highest)
		}
	}
}

// Who may schedule a replica count, and the codes of those who may not.
func TestScheduledReplicasRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		dep  store.Deployment
		role string
		want string
	}{
		{"single-node", store.Deployment{}, "", ""},
		{"single-node naming a role", store.Deployment{}, "decode", messages.ErrorScheduleInvalid},
		{"multi-node placed by the platform", store.Deployment{NNodes: 2, UnitsAuto: true}, "", ""},
		{"multi-node placed by hand", store.Deployment{NNodes: 2}, "", messages.ErrorScheduleReplicasManualPlacement},
		{"PD naming its role", store.Deployment{Placement: schedulePDPlacement, UnitsAuto: true}, "decode", ""},
		{"PD naming no role", store.Deployment{Placement: schedulePDPlacement, UnitsAuto: true}, "", messages.ErrorScheduleInvalid},
		{"PD naming a role its mode lacks", store.Deployment{Placement: schedulePDPlacement, UnitsAuto: true}, "encode", messages.ErrorScheduleInvalid},
		{"PD placed by hand", store.Deployment{Placement: schedulePDPlacement}, "decode", messages.ErrorScheduleReplicasManualPlacement},
	} {
		if got := scheduleCode(t, ScheduledReplicasRefusal(&tc.dep, tc.role)); got != tc.want {
			t.Fatalf("%s: code %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReplicasOutOfBoundsNamesTheRolesCode(t *testing.T) {
	if got := scheduleCode(t, ReplicasOutOfBounds("decode", 0, 1, 32)); got != messages.ErrorPDRoleUnitsEmpty {
		t.Fatalf("below = %q", got)
	}
	if got := scheduleCode(t, ReplicasOutOfBounds("decode", 33, 1, 32)); got != messages.ErrorPDRoleUnitsTooMany {
		t.Fatalf("above = %q", got)
	}
}

// What a schedule may order, per deployment and per PD role, as the
// scheduling views show it.
func TestScheduleCapabilitiesOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		dep  store.Deployment
		want ScheduleCapabilities
	}{
		{
			name: "a sleepable single-node deployment may order everything",
			dep:  store.Deployment{Engine: "vllm", Sleepable: true},
			want: ScheduleCapabilities{Replicas: []ReplicaCapability{{Lowest: 1, Highest: MaxReplicas}}},
		},
		{
			name: "one that cannot sleep only scales",
			dep:  store.Deployment{Engine: "vllm"},
			want: ScheduleCapabilities{Sleep: sleep.CodeNotEnabled, Wake: sleep.CodeNotEnabled, Replicas: []ReplicaCapability{{Lowest: 1, Highest: MaxReplicas}}},
		},
		{
			name: "multi-node placed by hand cannot scale by a count",
			dep:  store.Deployment{Engine: "vllm", Sleepable: true, NNodes: 2},
			want: ScheduleCapabilities{Replicas: []ReplicaCapability{{Lowest: 1, Highest: MaxReplicas, Refusal: messages.ErrorScheduleReplicasManualPlacement}}},
		},
		{
			name: "PD scales per role, in the mode's order",
			dep:  store.Deployment{Engine: "vllm", Sleepable: true, Placement: schedulePDPlacement, UnitsAuto: true},
			want: ScheduleCapabilities{Replicas: []ReplicaCapability{
				{Role: "decode", Lowest: 1, Highest: pdmode.MaxUnitsPerRole},
				{Role: "prefill", Lowest: 0, Highest: pdmode.MaxUnitsPerRole},
			}},
		},
	} {
		if got := ScheduleCapabilitiesOf(&tc.dep); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}
