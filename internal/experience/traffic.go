package experience

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/simulate"
)

// prompts are what the live traffic asks, so request records read like
// teams using their models.
var prompts = []string{
	"帮我写一段产品发布会的开场白",
	"把这段话翻译成英文：算力调度要兼顾利用率和稳定性",
	"总结一下 Transformer 的核心思想",
	"用 Python 写一个快速排序",
	"解释一下 KV Cache 为什么能加速推理",
	"给新员工写一封欢迎邮件",
	"列出三个提升 GPU 利用率的方法",
}

// traffic keeps a stream of requests on the running deployments through
// the gateway, from the callers' own API keys, the way their applications
// would: one to a few a second, as many as the hour's load calls for (Rhythm),
// shared among the deployments as their history is. Usage, request and
// engine metrics keep moving between the hours the history covers.
func traffic(ctx context.Context, base string, c *apiClient, keys map[string]string) {
	client := &http.Client{Timeout: 2 * time.Minute}
	inflight := make(chan struct{}, 64)
	var running []deployment
	refreshed := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		if time.Since(refreshed) > 30*time.Second {
			if list, err := listDeployments(ctx, c); err == nil {
				running = running[:0]
				for _, d := range list {
					if d.RuntimeState == "running" && d.ServedName != "" {
						running = append(running, d)
					}
				}
				refreshed = time.Now()
			}
		}
		if len(running) == 0 {
			continue
		}
		rate := 0.6 + 1.9*simulate.Rhythm(time.Now())
		for n := int(rate) + boolInt(rand.Float64() < rate-math.Floor(rate)); n > 0; n-- {
			d := pickDeployment(running)
			cl := pickCaller()
			select {
			case inflight <- struct{}{}:
			default:
				continue // the platform is busy enough
			}
			go func(model string, cl caller) {
				defer func() { <-inflight }()
				body, _ := json.Marshal(map[string]any{
					"model": model, "stream": true, "max_tokens": min(cl.Output, 400), "ignore_eos": true,
					"messages": []map[string]string{
						{"role": "system", "content": liveContext(cl)},
						{"role": "user", "content": prompts[rand.IntN(len(prompts))]},
					},
				})
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", strings.NewReader(string(body)))
				req.Header.Set("Authorization", "Bearer "+keys[cl.Name])
				req.Header.Set("Content-Type", "application/json")
				if resp, err := client.Do(req); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}(d.ServedName, cl)
		}
	}
}

// pickDeployment draws a running deployment by its starter's share of the
// traffic; one the visitor added takes an average share.
func pickDeployment(running []deployment) deployment {
	weights := make([]float64, len(running))
	total := 0.0
	for i, d := range running {
		weights[i] = 1.0 / float64(len(starters))
		for _, st := range starters {
			if st.Name == d.Name {
				weights[i] = st.Share
			}
		}
		total += weights[i]
	}
	r := rand.Float64() * total
	for i, w := range weights {
		if r -= w; r < 0 {
			return running[i]
		}
	}
	return running[len(running)-1]
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pickCaller draws a caller by its share of the traffic.
func pickCaller() caller {
	r := rand.Float64()
	for _, cl := range callers {
		if r -= cl.Share; r < 0 {
			return cl
		}
	}
	return callers[0]
}
