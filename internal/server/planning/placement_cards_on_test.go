package planning

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// An index names a card on one machine: a placement carried onto another node
// keeps the indexes that node reports and drops the ones it does not, so the
// launch is not held to a card this node never had (GWKS-522).
func TestCardsOnKeepsOnlyTheNodesOwnCards(t *testing.T) {
	twoCards := &store.Node{UUID: "two", GPUs: []store.GPU{{Index: 0}, {Index: 3}}}
	for _, tc := range []struct {
		name    string
		indexes []int
		want    []int
	}{
		{"both reported", []int{0, 3}, []int{0, 3}},
		{"one is another node's", []int{3, 7}, []int{3}},
		{"none reported", []int{7, 9}, []int{}},
		{"nothing pinned", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			placement := (Placement{GPUIndexes: tc.indexes, TP: 1}).CardsOn(twoCards)
			if !slices.Equal(placement.GPUIndexes, tc.want) {
				t.Fatalf("cards = %v, want %v", placement.GPUIndexes, tc.want)
			}
			if placement.TP != 1 {
				t.Fatalf("the rest of the placement changed: %+v", placement)
			}
		})
	}
}

// A preview has no deployment yet: its errors must not read "deployment 0".
func TestALaunchErrorWithoutADeploymentNamesNoDeployment(t *testing.T) {
	cause := errors.New("vllm topology: needs 2 GPUs, node has 1")
	preview := (&LaunchResolveError{Err: cause}).Error()
	if strings.Contains(preview, "deployment") || !strings.HasPrefix(preview, "resolve launch: vllm topology: ") {
		t.Fatalf("preview error = %q", preview)
	}
	stored := (&LaunchResolveError{DeploymentID: 42, Err: cause}).Error()
	if !strings.HasPrefix(stored, "deployment 42 resolve launch: vllm topology: ") {
		t.Fatalf("stored error = %q", stored)
	}
}
