package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The windows the sharing stats answer for are the server's, listed in the
// answer: 15 minutes to 24 hours, 6 hours when none is asked for; a yield
// counts in the windows it falls in. A node's lanes keep 6 and 24 hours.
func TestSharingStatsWindowsAreTheServers(t *testing.T) {
	r := newTimelineRig(t)
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", 0, `{"state":"running","phase":""}`)
	// One yield to the shared deployment two hours ago, one just now.
	r.event("", r.plain.ID, store.EventKindAutomation, "AUTO_SLEEP", time.Hour, fmt.Sprintf(`{"trigger":"preempt","reason":"turn","for_deployment":%d}`, r.shared.ID))
	r.st.AddDeploymentEventCode(r.plain.ID, store.EventKindAutomation, "AUTO_SLEEP", map[string]any{"trigger": "preempt", "reason": "turn", "for_deployment": r.shared.ID})
	status, body := r.get("?sharing=1")
	if status != http.StatusOK || !strings.Contains(body, `"key":"6h","seconds":21600},"windows":[{"key":"15m","seconds":900},{"key":"1h","seconds":3600},{"key":"6h","seconds":21600},{"key":"24h","seconds":86400}]`) {
		t.Fatalf("default = %d %s", status, body)
	}
	for window, want := range map[string]float64{"15m": 1, "1h": 1, "6h": 2, "24h": 2} {
		status, body := r.get("?sharing=1&window=" + window)
		row, ok := sharingRow(t, body, r.shared.ID)
		if status != http.StatusOK || !ok || row["yielded_to"] != want {
			t.Errorf("%s: %d yielded_to %v, want %v: %s", window, status, row["yielded_to"], want, body)
		}
	}
	var quarter dto.SchedulingTimeline
	_, body = r.get("?sharing=1&window=15m")
	if err := json.Unmarshal([]byte(body), &quarter); err != nil || quarter.Window.Hours != 0 || quarter.Window.Seconds != 900 || quarter.Window.To.Sub(quarter.Window.From) != 15*time.Minute {
		t.Fatalf("15m window = %+v %v", quarter.Window, err)
	}
	for query, field := range map[string]string{
		"?sharing=1&window=2h":                 "window",
		"?node=" + r.node.UUID + "&window=15m": "window",
		"?sharing=1&hours=12":                  "hours",
	} {
		status, body := r.get(query)
		if status != http.StatusBadRequest || !strings.Contains(body, `"field":"`+field+`"`) {
			t.Errorf("%s = %d %s, want 400 on %s", query, status, body, field)
		}
	}
	// A server without the activity answers the timeline 404.
	bare := &apiHandlers{d: Deps{Store: r.st}}
	if rec := callHandler(bare.schedulingTimeline, "/api/v1/scheduling/timeline?sharing=1"); rec.Code != http.StatusNotFound {
		t.Fatalf("without the activity = %d", rec.Code)
	}
}
