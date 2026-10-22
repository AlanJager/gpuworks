package pullref

import (
	"reflect"
	"strings"
	"testing"
)

const runner = "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2"

var runnerCopies = []string{
	"quay.io/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2",
	"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2",
}

func refsOf(t *testing.T, candidates []PlannedCandidate) []string {
	t.Helper()
	return CandidateRefs(candidates)
}

func TestCopiesOrderTemplateThenCatalogThenCanonical(t *testing.T) {
	got := Copies(runner,
		[]string{"ghcr.io/team/runner:v1", " "},
		append([]string{"https://dl.example/runner.tar", "not a ref", runnerCopies[0]}, runnerCopies...))
	want := []string{"ghcr.io/team/runner:v1", runnerCopies[0], runnerCopies[1], runner}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Copies = %#v, want %#v", got, want)
	}
	// An image the catalog does not know is its own only copy.
	if got := Copies("vllm/vllm-openai:v0.11.0", nil, nil); !reflect.DeepEqual(got, []string{"vllm/vllm-openai:v0.11.0"}) {
		t.Fatalf("Copies(uncatalogued) = %#v", got)
	}
}

func TestPlanCandidatesWithoutRulesRanksEveryCopy(t *testing.T) {
	copies := Copies(runner, nil, runnerCopies)
	got, err := PlanCandidates(runner, copies, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []PlannedCandidate{
		{Ref: runnerCopies[0], Source: SourceQuay, Rank: true},
		{Ref: runnerCopies[1], Source: SourceHuaweiCloud, Rank: true},
		{Ref: runner, Source: SourceDirect, Rank: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PlanCandidates = %#v, want %#v", got, want)
	}
}

func TestPlanCandidatesPrefixRuleGoesFirstAndKeepsUpstream(t *testing.T) {
	copies := Copies(runner, nil, runnerCopies)
	rules := []SourceRule{{Match: "gpuworks/", Endpoints: []string{"registry.zstack.ai/gpuworks/"}, AllowUpstream: true}}
	got, err := PlanCandidates(runner, copies, rules)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"registry.zstack.ai/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2",
		runnerCopies[0], runnerCopies[1], runner,
	}
	if !reflect.DeepEqual(refsOf(t, got), want) {
		t.Fatalf("refs = %#v, want %#v", refsOf(t, got), want)
	}
	if got[0].Rank || !got[1].Rank {
		t.Fatalf("rule endpoint must be unranked and copies ranked: %#v", got)
	}
}

func TestPlanCandidatesHostRuleKeepsRepositoryPathAndLibrary(t *testing.T) {
	rules := []SourceRule{{Match: "docker.io", Endpoints: []string{"harbor.corp.example/dockerhub-proxy", "docker.m.daocloud.io"}, AllowUpstream: true}}
	got, err := PlanCandidates("nginx:1.27", nil, rules)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"harbor.corp.example/dockerhub-proxy/library/nginx:1.27",
		"docker.m.daocloud.io/library/nginx:1.27",
		"nginx:1.27",
	}
	if !reflect.DeepEqual(refsOf(t, got), want) {
		t.Fatalf("refs = %#v, want %#v", refsOf(t, got), want)
	}
	// index.docker.io is Docker Hub under another name.
	got, err = PlanCandidates("index.docker.io/vllm/vllm-openai:v1", nil, rules)
	if err != nil || got[0].Ref != "harbor.corp.example/dockerhub-proxy/vllm/vllm-openai:v1" {
		t.Fatalf("index.docker.io not covered: %#v %v", got, err)
	}
}

func TestPlanCandidatesForbiddenUpstreamDropsTheCopyOnly(t *testing.T) {
	copies := Copies(runner, nil, runnerCopies)
	rules := []SourceRule{
		{Match: "docker.io", Endpoints: []string{"harbor.corp.example/dockerhub-proxy"}, AllowUpstream: false},
		{Match: "quay.io", Endpoints: []string{"harbor.corp.example/quay-proxy"}, AllowUpstream: false},
	}
	got, err := PlanCandidates(runner, copies, rules)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"harbor.corp.example/dockerhub-proxy/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2",
		"harbor.corp.example/quay-proxy/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2",
		// SWR is matched by no rule, so it stays reachable.
		runnerCopies[1],
	}
	if !reflect.DeepEqual(refsOf(t, got), want) {
		t.Fatalf("refs = %#v, want %#v", refsOf(t, got), want)
	}
}

func TestPlanCandidatesFailsWhenNothingIsAllowed(t *testing.T) {
	rules := []SourceRule{{Match: "ghcr.io", AllowUpstream: false}}
	if _, err := PlanCandidates("ghcr.io/team/app:v1", nil, rules); err == nil || !strings.Contains(err.Error(), "ghcr.io/team/app:v1") {
		t.Fatalf("err = %v, want a refusal naming the image", err)
	}
}

func TestPlanCandidatesDigestPinnedCopiesAreRewrittenToo(t *testing.T) {
	pinned := "ghcr.io/team/app@sha256:" + strings.Repeat("a", 64)
	rules := []SourceRule{{Match: "ghcr.io", Endpoints: []string{"ghcr.nju.edu.cn"}, AllowUpstream: true}}
	got, err := PlanCandidates(pinned, nil, rules)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ghcr.nju.edu.cn/team/app@sha256:" + strings.Repeat("a", 64), pinned}
	if !reflect.DeepEqual(refsOf(t, got), want) {
		t.Fatalf("refs = %#v, want %#v", refsOf(t, got), want)
	}
}

func TestPlanCandidatesRulesApplyInOrderAndDeduplicate(t *testing.T) {
	rules := []SourceRule{
		{Match: "vllm/", Endpoints: []string{"mirror.a/vllm"}, AllowUpstream: true},
		{Match: "docker.io", Endpoints: []string{"mirror.b", "mirror.a"}, AllowUpstream: true},
	}
	got, err := PlanCandidates("vllm/vllm-openai:v1", nil, rules)
	if err != nil {
		t.Fatal(err)
	}
	// mirror.a/vllm/vllm-openai:v1 is produced by both rules and kept once, in
	// the first rule's place.
	want := []string{"mirror.a/vllm/vllm-openai:v1", "mirror.b/vllm/vllm-openai:v1", "vllm/vllm-openai:v1"}
	if !reflect.DeepEqual(refsOf(t, got), want) {
		t.Fatalf("refs = %#v, want %#v", refsOf(t, got), want)
	}
}

func TestPrefixRuleMatchesOnSegmentBoundaries(t *testing.T) {
	rules := []SourceRule{{Match: "gpuworks", Endpoints: []string{"registry.zstack.ai/gpuworks"}, AllowUpstream: true}}
	got, err := PlanCandidates("gpuworks-extra/app:v1", nil, rules)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ref != "gpuworks-extra/app:v1" {
		t.Fatalf("prefix matched across a segment boundary: %#v", got)
	}
}

func TestValidateRuleMatchAndEndpoint(t *testing.T) {
	for _, ok := range []string{"docker.io", "gpuworks/", "quay.io/team", "registry.example:5000", "localhost:5000/team"} {
		if err := ValidateRuleMatch(ok); err != nil {
			t.Errorf("ValidateRuleMatch(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/", "https://docker.io", "team/app:v1", "a b", "team/app@sha256:x"} {
		if err := ValidateRuleMatch(bad); err == nil {
			t.Errorf("ValidateRuleMatch(%q) accepted", bad)
		}
	}
	for _, ok := range []string{"harbor.corp.example/dockerhub-proxy", "docker.m.daocloud.io", "registry.zstack.ai/gpuworks/", "localhost:5000"} {
		if err := ValidateRuleEndpoint(ok); err != nil {
			t.Errorf("ValidateRuleEndpoint(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "proxy/dockerhub", "https://harbor", "harbor.example/x:tag"} {
		if err := ValidateRuleEndpoint(bad); err == nil {
			t.Errorf("ValidateRuleEndpoint(%q) accepted", bad)
		}
	}
}
