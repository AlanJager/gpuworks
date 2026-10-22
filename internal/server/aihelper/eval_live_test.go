//go:build aieval

package aihelper

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLiveEval diagnoses every recorded failure against a real endpoint in
// three ways - tools only (the old deep mode), seed only (one call, no tools)
// and seed plus tools (what the product runs) - and reports how many expected
// fix keywords each answer hits, the LLM calls, tool calls, tokens and time.
//
//	GPUWORKS_AI_EVAL_BASE_URL=https://api.deepseek.com GPUWORKS_AI_EVAL_API_KEY=... \
//	GPUWORKS_AI_EVAL_MODEL=deepseek-v4-flash \
//	go test -tags aieval -count=1 -v -timeout 60m ./internal/server/aihelper/ -run '^TestLiveEval$'
//
// GPUWORKS_AI_EVAL_OUT, when set, receives the table as Markdown;
// GPUWORKS_AI_EVAL_MODES (comma-separated mode names) runs a subset;
// GPUWORKS_AI_EVAL_SYSTEM_FILE replaces the diagnosis system prompt, to
// compare a prompt change against the current one.
func TestLiveEval(t *testing.T) {
	ep := Endpoint{BaseURL: os.Getenv("GPUWORKS_AI_EVAL_BASE_URL"), APIKey: os.Getenv("GPUWORKS_AI_EVAL_API_KEY"), Model: os.Getenv("GPUWORKS_AI_EVAL_MODEL")}
	if ep.BaseURL == "" || ep.Model == "" {
		t.Fatal("set GPUWORKS_AI_EVAL_BASE_URL, GPUWORKS_AI_EVAL_API_KEY and GPUWORKS_AI_EVAL_MODEL")
	}
	type mode struct {
		name  string
		seed  bool
		steps int
	}
	modes := []mode{{"tools-only", false, DefaultMaxSteps}, {"seed-only", true, 0}, {"seed+tools", true, DefaultMaxSteps}}
	if only := os.Getenv("GPUWORKS_AI_EVAL_MODES"); only != "" {
		modes = slices.DeleteFunc(modes, func(m mode) bool { return !slices.Contains(strings.Split(only, ","), m.name) })
	}
	type total struct {
		hit, groups, calls, tools, tokens int
		elapsed                           time.Duration
		failed                            int
	}
	totals := map[string]*total{}
	system := SystemErrorDiagnosis
	if path := os.Getenv("GPUWORKS_AI_EVAL_SYSTEM_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		system = strings.TrimSpace(string(raw))
	}
	var rows []string
	for _, c := range loadEvalCases(t) {
		for _, m := range modes {
			env := &caseEnv{c: c}
			prompt := c.bareprompt()
			if m.seed {
				prompt = c.seed().Prompt()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			start := time.Now()
			res, err := NewRunner(NewClient(ep), env).Run(ctx, WithResponseLanguage(system, "en", false), prompt, m.steps)
			cancel()
			tt := totals[m.name]
			if tt == nil {
				tt = &total{}
				totals[m.name] = tt
			}
			elapsed := time.Since(start)
			hit := 0
			if err != nil {
				tt.failed++
			} else {
				hit = keywordScore(res.Answer, c.ExpectKeywords)
			}
			tt.hit += hit
			tt.groups += len(c.ExpectKeywords)
			tt.calls += res.Calls
			tt.tools += len(res.Steps)
			tt.tokens += res.Usage.PromptTokens + res.Usage.CompletionTokens
			tt.elapsed += elapsed
			rows = append(rows, fmt.Sprintf("| %s | %s | %d/%d | %d | %d | %d | %.1fs | %v |", c.ID, m.name, hit, len(c.ExpectKeywords), res.Calls, len(res.Steps), res.Usage.PromptTokens+res.Usage.CompletionTokens, elapsed.Seconds(), err))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Model: %s\n\n| mode | keyword hits | LLM calls | tool calls | tokens | time | errors |\n|---|---|---|---|---|---|---|\n", ep.Model)
	for _, m := range modes {
		tt := totals[m.name]
		fmt.Fprintf(&b, "| %s | %d/%d (%.0f%%) | %d | %d | %d | %.1fs | %d |\n", m.name, tt.hit, tt.groups, 100*float64(tt.hit)/float64(max(tt.groups, 1)), tt.calls, tt.tools, tt.tokens, tt.elapsed.Seconds(), tt.failed)
	}
	b.WriteString("\n| case | mode | hits | calls | tools | tokens | time | error |\n|---|---|---|---|---|---|---|---|\n")
	b.WriteString(strings.Join(rows, "\n") + "\n")
	t.Log("\n" + b.String())
	if out := os.Getenv("GPUWORKS_AI_EVAL_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const evalInstance = "eval-instance"

// bareprompt is what the old deep mode sent: references and the failure
// message, everything else left to tools.
func (c evalCase) bareprompt() string {
	return fmt.Sprintf("Diagnose why this instance failed and how to fix it.\ninstance_uuid: %s\nnode_uuid: eval-node\ndeployment_id: 1\nfailure message: %s\nlaunch command: %s\n", evalInstance, c.Message, strings.Join(c.Command, " "))
}

// seed builds the context the server would gather, from the recorded case.
func (c evalCase) seed() ErrorContext {
	logs := make([]LogLine, len(c.Logs))
	for i, l := range c.Logs {
		logs[i] = LogLine{Line: l}
	}
	return ErrorContext{
		InstanceUUID: evalInstance, DeploymentID: 1, DeploymentName: "eval", NodeUUID: "eval-node", State: "failed",
		Message: c.Message, Command: c.Command, Node: c.GPUs, Logs: logs, TailLines: 60, ErrorLines: 40,
		Findings: Match(c.lines()),
	}
}

// caseEnv answers the tools from the recorded case. The platform API is not
// part of a recorded case: every read through it answers 503, as a server
// whose API is down would, so the API tools report it and the model works
// from the seed and the two findings below.
type caseEnv struct{ c evalCase }

func (e *caseEnv) API() APIReader                  { return unavailableAPI{} }
func (e *caseEnv) GPUTopology(string) (any, error) { return map[string]any{"gpus": e.c.GPUs}, nil }
func (e *caseEnv) CompareWithHealthyReplica(string) (any, error) {
	return map[string]any{"healthy_replica": nil, "note": "no running replica of this deployment to compare with"}, nil
}

// unavailableAPI routes like the fixture router and answers every read 503.
type unavailableAPI struct{}

func (unavailableAPI) Routes() []string         { return newFakeAPI().Routes() }
func (unavailableAPI) Route(path string) string { return newFakeAPI().Route(path) }
func (unavailableAPI) Get(_ context.Context, path string, _ url.Values) (int, []byte, error) {
	return http.StatusServiceUnavailable, []byte(`{"error":"the platform API is not part of a recorded eval case"}`), nil
}
