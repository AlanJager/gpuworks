package sourceurl

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const hf = "https://huggingface.co"

func TestIsURLTellsSiteRulesFromRegistryRules(t *testing.T) {
	for match, want := range map[string]bool{
		"https://huggingface.co":   true,
		" HTTP://mirror.corp:8090": true,
		"docker.io":                false,
		"gpuworks/":                false,
		"quay.io/team/app":         false,
		"":                         false,
	} {
		if got := IsURL(match); got != want {
			t.Errorf("IsURL(%q) = %v, want %v", match, got, want)
		}
	}
}

func TestNormalizeSpellsAddressesOneWay(t *testing.T) {
	for raw, want := range map[string]string{
		"https://HuggingFace.co/":          "https://huggingface.co",
		"HTTPS://huggingface.co:443":       "https://huggingface.co",
		"http://hf-proxy.corp:80/":         "http://hf-proxy.corp",
		"http://hf-proxy.corp:8090":        "http://hf-proxy.corp:8090",
		"https://xget.example/HF/":         "https://xget.example/HF",
		"http://[fd00::1]:8090/mirror":     "http://[fd00::1]:8090/mirror",
		"  https://modelscope.cn  ":        "https://modelscope.cn",
		"https://mirror.example/a%20b/":    "https://mirror.example/a%20b",
		"https://mirror.example/hf/models": "https://mirror.example/hf/models",
	} {
		got, err := Normalize(raw)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestNormalizeRefusesWhatCannotBeAPrefix(t *testing.T) {
	for raw, wantErr := range map[string]string{
		"":                                   "empty",
		"huggingface.co":                     "http:// or https://",
		"ftp://mirror.example":               "http:// or https://",
		"https://user:secret@mirror.example": "credential",
		"https://mirror.example/?token=x":    "query or fragment",
		"https://mirror.example/?":           "query or fragment",
		"https://mirror.example/#frag":       "query or fragment",
		"https://mirror .example":            "whitespace",
		"https:///path":                      "no host",
	} {
		_, err := Normalize(raw)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Normalize(%q) error = %v, want one mentioning %q", raw, err, wantErr)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("Normalize(%q) error repeats the password: %v", raw, err)
		}
	}
}

func TestValidateMatchIsASiteRootButEndpointsMayHavePaths(t *testing.T) {
	if err := ValidateMatch("https://huggingface.co/"); err != nil {
		t.Fatalf("site root refused: %v", err)
	}
	if err := ValidateMatch("https://huggingface.co/api"); err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("a match with a path would never apply and must be refused, got %v", err)
	}
	if err := ValidateEndpoint("https://xget.example/hf"); err != nil {
		t.Fatalf("endpoint with a path refused: %v", err)
	}
	if err := ValidateEndpoint("docker.io"); err == nil {
		t.Fatal("a registry host is not an HTTP endpoint")
	}
}

func TestHostIsWhatCredentialsAreKeyedBy(t *testing.T) {
	for address, want := range map[string]string{
		"https://huggingface.co/api/models/x": "huggingface.co",
		"http://hf-proxy.corp:8090/":          "hf-proxy.corp:8090",
		"https://HF-Mirror.com:443":           "hf-mirror.com",
		"not a url":                           "",
	} {
		if got := Host(address); got != want {
			t.Errorf("Host(%q) = %q, want %q", address, got, want)
		}
	}
}

func TestPlanWithoutRulesIsTheSiteItself(t *testing.T) {
	got, err := Plan(hf+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Candidate{{Base: hf, Upstream: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Plan = %#v, want %#v", got, want)
	}
}

func TestPlanPutsEndpointsFirstRuleByRuleThenTheSite(t *testing.T) {
	rules := []Rule{
		{Match: "https://modelscope.cn", Endpoints: []string{"http://ms.corp"}, AllowUpstream: true},
		{Match: hf, Endpoints: []string{"http://hf-proxy.corp:8090", "https://hf-mirror.com/"}, AllowUpstream: true},
		{Match: "https://HUGGINGFACE.co", Endpoints: []string{"https://xget.example/hf", "https://hf-mirror.com"}, AllowUpstream: true},
	}
	got, err := Plan(hf, rules)
	if err != nil {
		t.Fatal(err)
	}
	want := []Candidate{
		{Base: "http://hf-proxy.corp:8090"},
		{Base: "https://hf-mirror.com"},
		{Base: "https://xget.example/hf"},
		{Base: hf, Upstream: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Plan = %#v, want %#v (a rule for another site contributes nothing; a repeated endpoint keeps its first place)", got, want)
	}
	if bases := Bases(got); bases[0] != "http://hf-proxy.corp:8090" || len(bases) != 4 {
		t.Fatalf("Bases = %v", bases)
	}
}

func TestPlanMatchesOnHostBoundariesOnly(t *testing.T) {
	rules := []Rule{{Match: hf, Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: false}}
	for _, base := range []string{"https://huggingface.co.evil.example", "https://cdn-avatars.huggingface.co", "http://huggingface.co"} {
		got, err := Plan(base, rules)
		if err != nil {
			t.Fatalf("Plan(%q): %v", base, err)
		}
		if len(got) != 1 || !got[0].Upstream {
			t.Fatalf("Plan(%q) = %#v: a rule for %s must not apply to another host or scheme", base, got, hf)
		}
	}
}

func TestPlanKeepsWhatFollowsTheSiteRoot(t *testing.T) {
	rules := []Rule{{Match: hf, Endpoints: []string{"https://xget.example/hf"}, AllowUpstream: true}}
	got, err := Plan(hf+"/org", rules)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Base != "https://xget.example/hf/org" {
		t.Fatalf("first candidate = %q", got[0].Base)
	}
}

func TestPlanLeavesOutAForbiddenSite(t *testing.T) {
	rules := []Rule{
		{Match: hf, Endpoints: []string{"http://hf-proxy.corp:8090"}, AllowUpstream: true},
		{Match: hf, Endpoints: nil, AllowUpstream: false},
	}
	got, err := Plan(hf, rules)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Candidate{{Base: "http://hf-proxy.corp:8090"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Plan = %#v, want %#v: any matching rule that forbids the site leaves it out", got, want)
	}
}

func TestPlanWithNothingAllowedIsAnError(t *testing.T) {
	_, err := Plan(hf, []Rule{{Match: hf, AllowUpstream: false}})
	if !errors.Is(err, ErrNoAllowedSource) {
		t.Fatalf("err = %v, want ErrNoAllowedSource", err)
	}
	if !strings.Contains(err.Error(), hf) {
		t.Fatalf("the error names the site: %v", err)
	}
}

func TestPlanSkipsAnEndpointThatDoesNotParse(t *testing.T) {
	got, err := Plan(hf, []Rule{{Match: hf, Endpoints: []string{"not a url", "https://hf-mirror.com"}, AllowUpstream: false}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Candidate{{Base: "https://hf-mirror.com"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Plan = %#v, want %#v", got, want)
	}
}

func TestPlanRefusesABaseThatIsNotAnAddress(t *testing.T) {
	if _, err := Plan("Qwen/Qwen3-8B", nil); err == nil {
		t.Fatal("a repository id is not a base address")
	}
}
