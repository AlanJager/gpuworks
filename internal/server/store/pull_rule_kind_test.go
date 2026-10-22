package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSiteRulesAreStoredInTheirNormalSpelling(t *testing.T) {
	s := newTestStore(t)
	rule, err := s.CreatePullSourceRule(PullSourceRuleSpec{
		Match:         " HTTPS://HuggingFace.co/ ",
		Endpoints:     []string{"http://hf-proxy.corp:8090/", " ", "https://hf-mirror.com:443"},
		AllowUpstream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rule.Kind() != PullRuleURL {
		t.Fatalf("kind = %q, want %q", rule.Kind(), PullRuleURL)
	}
	if rule.Match != "https://huggingface.co" || !reflect.DeepEqual(rule.Endpoints, []string{"http://hf-proxy.corp:8090", "https://hf-mirror.com"}) {
		t.Fatalf("stored rule = %#v", rule)
	}
	rules, err := s.PullSourceRules()
	if err != nil || len(rules) != 1 || !reflect.DeepEqual(rules[0].Endpoints, rule.Endpoints) {
		t.Fatalf("read back %#v, err %v", rules, err)
	}
	if got := rules[0].URLRule(); got.Match != rule.Match || !got.AllowUpstream || len(got.Endpoints) != 2 {
		t.Fatalf("URLRule = %#v", got)
	}

	image, err := s.CreatePullSourceRule(PullSourceRuleSpec{Match: "docker.io", Endpoints: []string{"harbor.corp/dockerhub"}, AllowUpstream: true})
	if err != nil {
		t.Fatal(err)
	}
	if image.Kind() != PullRuleImage {
		t.Fatalf("a registry rule is of kind %q", image.Kind())
	}
}

func TestSiteRulesRefuseWhatTheyCannotUse(t *testing.T) {
	s := newTestStore(t)
	for name, tc := range map[string]struct {
		spec  PullSourceRuleSpec
		field string
		says  string
	}{
		"registry endpoint for a site": {PullSourceRuleSpec{Match: "https://huggingface.co", Endpoints: []string{"hf-mirror.com"}, AllowUpstream: true}, "endpoints", "not an http(s) address"},
		"site endpoint for a registry": {PullSourceRuleSpec{Match: "docker.io", Endpoints: []string{"https://mirror.example"}, AllowUpstream: true}, "endpoints", "URL"},
		"match with a path":            {PullSourceRuleSpec{Match: "https://huggingface.co/api", Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: true}, "match", "path"},
		"credentials in an endpoint":   {PullSourceRuleSpec{Match: "https://huggingface.co", Endpoints: []string{"https://u:secret@mirror.example"}, AllowUpstream: true}, "endpoints", "credential"},
		"same endpoint twice":          {PullSourceRuleSpec{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.com", "HTTPS://hf-mirror.com/"}, AllowUpstream: true}, "endpoints", "twice"},
		"nothing to try":               {PullSourceRuleSpec{Match: "https://huggingface.co", AllowUpstream: true}, "endpoints", "at least one endpoint"},
	} {
		_, err := s.CreatePullSourceRule(tc.spec)
		var refused *PullSettingError
		if !errors.As(err, &refused) || refused.Field != tc.field || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: err = %v, want a %s error saying %q", name, err, tc.field, tc.says)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: the error repeats a password: %v", name, err)
		}
	}
	// No endpoint with the site closed is how a rule forbids it.
	forbid, err := s.CreatePullSourceRule(PullSourceRuleSpec{Match: "https://modelscope.cn", AllowUpstream: false})
	if err != nil || len(forbid.Endpoints) != 0 {
		t.Fatalf("a forbidding rule = %#v, %v", forbid, err)
	}
	if rules, _ := s.PullSourceRules(); len(rules) != 1 {
		t.Fatalf("refused rules were stored: %#v", rules)
	}
}

func TestLegacyModelEndpointRewrite(t *testing.T) {
	from, to, err := modelEndpointRewrite(" https://hf-mirror.example/ ")
	if err != nil || from != "https://huggingface.co" || to != "https://hf-mirror.example" {
		t.Fatalf("rewrite = %q → %q, err = %v", from, to, err)
	}
	for _, bad := range []string{"", "mirror.example", "://x"} {
		if _, _, err := modelEndpointRewrite(bad); err == nil {
			t.Fatalf("endpoint %q accepted", bad)
		}
	}
}

// The control plane reads sites, never registries: only a site rule may be
// chosen for it, and the choice round-trips.
func TestOnlySiteRulesServeTheControlPlane(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	_, err := st.CreatePullSourceRule(PullSourceRuleSpec{Match: "docker.io", Endpoints: []string{"harbor.corp/hub"}, AllowUpstream: true, ControlPlane: true})
	var invalid *PullSettingError
	if !errors.As(err, &invalid) || invalid.Field != "control_plane" {
		t.Fatalf("image rule for the control plane: err = %v", err)
	}
	rule, err := st.CreatePullSourceRule(PullSourceRuleSpec{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: true, ControlPlane: true})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := st.PullSourceRules()
	if err != nil || len(rules) != 1 || !rules[0].ControlPlane {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	rule.ControlPlane = false
	if _, err := st.UpdatePullSourceRule(rule.ID, rule); err != nil {
		t.Fatal(err)
	}
	if rules, _ := st.PullSourceRules(); rules[0].ControlPlane {
		t.Fatal("clearing the choice did not stick")
	}
}
