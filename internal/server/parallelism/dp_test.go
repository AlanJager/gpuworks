package parallelism

import "testing"

func TestGPUAllocationIncludesDataParallelCopies(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want int
	}{
		{"one node two copies", Spec{TP: 1, PP: 1, DP: 2}, 2},
		{"one node four cards", Spec{TP: 2, PP: 1, DP: 2}, 4},
		{"two nodes split copies", Spec{TP: 2, PP: 1, DP: 2, NNodes: 2}, 2},
		{"default unchanged", Spec{TP: 2}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.GPUsPerNode()
			if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
			if tc.spec.World() != tc.spec.Normalized().TP*tc.spec.Normalized().PP {
				t.Fatal("DP replicas diluted per-copy weights")
			}
		})
	}
}

func TestDataParallelDoesNotInventFractionalReplica(t *testing.T) {
	spec := (Spec{DP: 2, NNodes: 2}).Defaulted([]int{1, 1})
	if spec.TP != 1 || spec.PP != 1 {
		t.Fatalf("one DP replica per node should fit: %+v", spec)
	}
	if need, err := spec.GPUsPerNode(); err != nil || need != 1 {
		t.Fatalf("need = %d, %v", need, err)
	}
	blocked := (Spec{PP: 2, DP: 2, NNodes: 2}).Defaulted([]int{1, 1})
	if blocked.TP != 0 {
		t.Fatalf("invented fractional TP for explicit PP: %+v", blocked)
	}
	if _, err := blocked.GPUsPerNode(); err != ErrTPRequired {
		t.Fatalf("want refusal, got %+v, %v", blocked, err)
	}
	assigned := (Spec{DP: 2}).Assigned(1)
	if assigned.TP != 1 {
		t.Fatalf("assigned fractional replica: %+v", assigned)
	}
	if need, err := assigned.GPUsPerNode(); err != nil || need != 2 {
		t.Fatalf("need = %d, %v", need, err)
	}
}
