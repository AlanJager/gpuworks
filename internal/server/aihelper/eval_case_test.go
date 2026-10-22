package aihelper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// evalCase is one recorded engine failure (testdata/eval). The offline test
// below checks the known-failure table against every case; eval_live_test.go
// (build tag aieval) asks a real endpoint to diagnose them.
type evalCase struct {
	ID      string   `json:"id"`
	Source  string   `json:"source"`
	Note    string   `json:"note"`
	Message string   `json:"message"`
	Command []string `json:"command"`
	GPUs    []struct {
		Name      string `json:"name"`
		MemoryGiB int    `json:"memory_gib"`
	} `json:"gpus"`
	Logs            []string   `json:"logs"`
	ExpectSignature string     `json:"expect_signature"`
	ExpectKeywords  [][]string `json:"expect_keywords"`
}

func loadEvalCases(t *testing.T) []evalCase {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "eval", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no eval cases: %v", err)
	}
	var cases []evalCase
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var c evalCase
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if c.ID+".json" != filepath.Base(p) || c.Source == "" || len(c.Logs) == 0 || len(c.ExpectKeywords) == 0 {
			t.Fatalf("%s: id, source, logs and expect_keywords are required", p)
		}
		cases = append(cases, c)
	}
	return cases
}

func (c evalCase) lines() []string { return append([]string{c.Message}, c.Logs...) }

// Every case with a known signature is recognised; a long-tail case raises no
// misleading hypothesis.
func TestEvalCasesMatchTheirSignature(t *testing.T) {
	codes := SignatureCodes()
	for _, c := range loadEvalCases(t) {
		var got []string
		for _, f := range Match(c.lines()) {
			got = append(got, f.Code)
		}
		if c.ExpectSignature == "" {
			if len(got) > 0 {
				t.Errorf("%s: long-tail case raised hypotheses %v", c.ID, got)
			}
			continue
		}
		if !slices.Contains(codes, c.ExpectSignature) {
			t.Errorf("%s: expect_signature %q is not a known code", c.ID, c.ExpectSignature)
		}
		if !slices.Contains(got, c.ExpectSignature) {
			t.Errorf("%s: Match = %v, want %s", c.ID, got, c.ExpectSignature)
		}
	}
}

// keywordScore reports how many expected keyword groups an answer hits; a
// group is hit when any of its synonyms appears (case-insensitive).
func keywordScore(answer string, groups [][]string) (hit int) {
	lower := strings.ToLower(answer)
	for _, g := range groups {
		for _, k := range g {
			if strings.Contains(lower, strings.ToLower(k)) {
				hit++
				break
			}
		}
	}
	return hit
}

func TestKeywordScore(t *testing.T) {
	if got := keywordScore("Lower --max-model-len and raise TP", [][]string{{"max_model_len", "max-model-len"}, {"tensor-parallel", "TP"}, {"quantize"}}); got != 2 {
		t.Fatalf("score = %d", got)
	}
}
