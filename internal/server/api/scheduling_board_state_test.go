package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A board card carries the state the overview gives the same card, in the
// board's wire shape: a card held outside the platform, one only a sleeper's
// residue is on and a free one, and on another node a faulty card - which the
// board used to leave unsaid.
func TestSchedulingBoardCardsCarryTheOverviewState(t *testing.T) {
	rig := newOverviewRig(t)
	d, err := rig.st.RegisterNode("gpu-d", store.HashToken("gpu-d"), "gpu-d")
	if err != nil {
		t.Fatal(err)
	}
	faulty := store.GPU{NodeID: d.ID, Ident: "gpu-d-0", Index: 0, Name: "L20", MemoryTotal: 48 * gib, HealthStatus: accelerator.HealthError}
	if err := rig.st.DB().Create(&faulty).Error; err != nil {
		t.Fatal(err)
	}

	gpus := func(node string) string {
		t.Helper()
		rec := rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/board?node="+node, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("board of %s = %d %s", node, rec.Code, rec.Body)
		}
		var raw struct {
			Nodes struct {
				Items []struct {
					GPUs json.RawMessage `json:"gpus"`
				} `json:"items"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw.Nodes.Items) != 1 {
			t.Fatalf("board of %s = %s (%v)", node, rec.Body, err)
		}
		return string(raw.Nodes.Items[0].GPUs)
	}
	h100 := int64(80 * gib)
	want := fmt.Sprintf(`[{"index":0,"name":"H100","memory_bytes":%d,"awake_bytes":0,"sleeping_bytes":0,"free_bytes":%d,"state":"held_outside","held_outside":true,"switching":false,"occupants":[]},`+
		`{"index":1,"name":"H100","memory_bytes":%d,"awake_bytes":0,"sleeping_bytes":%d,"free_bytes":%d,"state":"asleep","held_outside":false,"switching":false,`+
		`"occupants":[{"deployment_id":%d,"deployment":"night","yield_policy":"idle","instance":"night-c","state":"asleep","sleep_level":2,"claim_bytes":%d,"residue_bytes":%d}]},`+
		`{"index":2,"name":"H100","memory_bytes":%d,"awake_bytes":0,"sleeping_bytes":0,"free_bytes":%d,"state":"free","held_outside":false,"switching":false,"occupants":[]}]`,
		h100, h100, h100, 4*gib, h100-4*gib, rig.night.ID, h100, 4*gib, h100, h100)
	if got := gpus("gpu-c"); got != want {
		t.Fatalf("gpu-c cards =\n%s\nwant\n%s", got, want)
	}
	if got := gpus("gpu-d"); !strings.Contains(got, `"state":"unhealthy","held_outside":false`) {
		t.Fatalf("a faulty card = %s", got)
	}

	// Every card of every node: the board's state is the overview's.
	out, _ := rig.overview(t)
	for _, node := range out.Nodes {
		rec := rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/board?node="+node.UUID, "")
		var board dto.SchedulingBoard
		if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil || len(board.Nodes.Items) != 1 {
			t.Fatalf("board of %s = %s", node.UUID, rec.Body)
		}
		boardCards := board.Nodes.Items[0].GPUs
		if len(boardCards) != len(node.Cards) {
			t.Fatalf("%s: board %d cards, overview %d", node.UUID, len(boardCards), len(node.Cards))
		}
		for i, card := range node.Cards {
			if got, want := boardCards[i].State, out.States[card.State]; got != want || boardCards[i].Switching != card.Switching {
				t.Errorf("%s card %d: board %s switching=%v, overview %s switching=%v", node.UUID, card.Index, got, boardCards[i].Switching, want, card.Switching)
			}
		}
	}
	assertCardStatesMatchSummary(t, out)
	if out.Summary.Unhealthy != 1 {
		t.Fatalf("summary = %+v, want the faulty card counted unhealthy", out.Summary)
	}
}
