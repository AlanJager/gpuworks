package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// probeSleepState reads vLLM's boolean sleep endpoint after adopting a running
// container. The endpoint does not expose the exact level; callers must treat
// the result as only an asleep/awake fact.
func (e *Engine) probeSleepState(ctx context.Context, spec proto.InstanceDesiredState) (int, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/is_sleeping", spec.APIPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("sleep probe: http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, err
	}
	var b bool
	if json.Unmarshal(body, &b) == nil {
		if b {
			return 1, nil
		}
		return 0, nil
	}
	var obj struct {
		IsSleeping bool `json:"is_sleeping"`
		Sleeping   bool `json:"sleeping"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return 0, fmt.Errorf("invalid sleep probe response")
	}
	if obj.IsSleeping || obj.Sleeping {
		return 1, nil
	}
	return 0, nil
}
