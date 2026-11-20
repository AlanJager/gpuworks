package slurmboot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
)

// The setup script and provider.ParseSlurmApply share a contract written only
// in setup.sh's header: the GWKS_SLURM step=... markers. These tests run the
// real script and hand its output to the real parser, so a marker renamed on
// one side fails here rather than on a customer's cluster.

// parsed runs the setup with args and parses its output in mode.
func (w *world) parsed(t *testing.T, mode string, args ...string) (provider.SlurmApplyResult, *provider.SlurmApplyFailedError) {
	t.Helper()
	r := w.runSetup(args...)
	res, err := provider.ParseSlurmApply(mode, r.stdout, "")
	if err == nil {
		if r.code != 0 {
			t.Fatalf("%v: exit %d but the parser saw success\n%s", args, r.code, r.stdout)
		}
		return res, nil
	}
	var ferr *provider.SlurmApplyFailedError
	if !errors.As(err, &ferr) {
		t.Fatalf("%v: %v", args, err)
	}
	return res, ferr
}

// Every mode the platform runs parses as what the script did: setup, its
// check, uninstalling the agents and removing the health check, then
// removing again (nothing to do).
func TestTheParserReadsWhatTheScriptDid(t *testing.T) {
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		res, ferr := w.parsed(t, provider.SlurmModeCheck, "--check")
		if ferr != nil || res.Plan == nil {
			t.Fatalf("check: %+v %v", res, ferr)
		}
		if p := res.Plan; !p.Shared || p.ConfPath != w.confPath() || !p.ConfChanged || p.Interval <= 0 || p.SlurmCluster == "" {
			t.Fatalf("check plan: %+v", *p)
		}
		res, ferr = w.parsed(t, provider.SlurmModeSetup)
		if ferr != nil || res.Done != "ok" || !res.Shared {
			t.Fatalf("setup: %+v %v", res, ferr)
		}
		var ids []string
		for _, step := range res.Steps {
			ids = append(ids, step.ID)
			if step.Status == "" {
				t.Errorf("setup step %s has no status", step.ID)
			}
		}
		want := []string{provider.SlurmApplyIDGres, provider.SlurmApplyIDCheckScript, provider.SlurmApplyIDDistribute, provider.SlurmApplyIDSlurmConf, provider.SlurmApplyIDReconfigure, provider.SlurmApplyIDResume}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("setup steps = %v, want %v", ids, want)
		}
		res, ferr = w.parsed(t, provider.SlurmModeUninstallAgents, "--uninstall-agents")
		if ferr != nil || res.IntervalSeconds <= 0 {
			t.Fatalf("uninstall agents: %+v %v", res, ferr)
		}
		res, ferr = w.parsed(t, provider.SlurmModeRemoveCheck, "--remove")
		if ferr != nil || res.RemoveNoop {
			t.Fatalf("remove: %+v %v", res, ferr)
		}
		res, ferr = w.parsed(t, provider.SlurmModeRemoveCheck, "--remove")
		if ferr != nil || !res.RemoveNoop {
			t.Fatalf("remove again: %+v %v", res, ferr)
		}
	})
}

// A failure parses to the reason the UI has text for: a precheck code, or
// the step that failed.
func TestTheParserReadsWhyTheScriptFailed(t *testing.T) {
	keys := uiKeys(t, "slurmApplyReason")
	eachShell(t, func(t *testing.T, w *world) {
		w.controller("nfs", baseConf)
		_, ferr := w.parsed(t, provider.SlurmModeUninstallAgents, "--uninstall-agents")
		if ferr == nil || ferr.Reason != "uninstall_agents_failed" {
			t.Fatalf("uninstalling before setup: %+v", ferr)
		}
		mustMkdir(t, w.path("opt"), 0o755)
		mustChmod(t, w.path("opt"), 0o777)
		_, ferr = w.parsed(t, provider.SlurmModeSetup)
		if ferr == nil || ferr.Reason != "script_dir_unsafe" {
			t.Fatalf("an unsafe script directory: %+v", ferr)
		}
		for lang, k := range keys {
			for _, reason := range []string{"uninstall_agents_failed", "script_dir_unsafe"} {
				if !k[reason] {
					t.Errorf("%s has no text for %s", lang, reason)
				}
			}
		}
	})
}

// scriptFailures reads every failure reason the scripts can produce from
// their marker lines, as provider.ParseSlurmApply reads them: each precheck
// code (pre_fail <code>, reason=<code>) and "<step>_failed" for each other
// step that marks itself failed, whatever the order of its keys. The script
// directory's step is always followed by pre_fail script_dir_unsafe; done,
// distribute and server_reach are not failures of the run (the parser passes
// over them). It also returns every step the markers name.
func scriptFailures(t *testing.T) (codes, steps map[string]bool) {
	t.Helper()
	codes, steps = map[string]bool{}, map[string]bool{}
	preFail := regexp.MustCompile(`pre_fail ([a-z_]+) "`)
	markLine := regexp.MustCompile(`mark "([^"]*)"`)
	for _, s := range []string{setupTemplate, setupConfPart, setupNodesPart, setupReachPart, setupCheckPart, setupDirPart} {
		for _, m := range preFail.FindAllStringSubmatch(s, -1) {
			codes[m[1]] = true
		}
		for _, m := range markLine.FindAllStringSubmatch(s, -1) {
			kv := map[string]string{}
			for _, field := range strings.Fields(m[1]) {
				if k, v, ok := strings.Cut(field, "="); ok {
					kv[k] = v
				}
			}
			step := kv["step"]
			if step == "" || strings.ContainsAny(step, "$") {
				continue
			}
			steps[step] = true
			if kv["status"] != "failed" {
				continue
			}
			switch step {
			case "precheck":
				if r := kv["reason"]; r != "" && !strings.ContainsAny(r, "$") {
					codes[r] = true
				}
			case "script_dir", "done", "distribute", "server_reach":
			default:
				codes[step+"_failed"] = true
			}
		}
	}
	if len(codes) < 10 || len(steps) < 15 {
		t.Fatalf("found only %d failure codes and %d steps in the scripts: the patterns no longer match", len(codes), len(steps))
	}
	return codes, steps
}

// Every failure reason the scripts can produce has text in both languages,
// and every "<step>_failed" text is for a failure some marker can produce
// (a step the scripts name that never marks itself failed would show
// unknown instead: seen with slurm_conf).
func TestEveryScriptFailureHasText(t *testing.T) {
	codes, steps := scriptFailures(t)
	var missing, dead []string
	for lang, k := range uiKeys(t, "slurmApplyReason") {
		for c := range codes {
			if !k[c] {
				missing = append(missing, lang+": "+c)
			}
		}
		for key := range k {
			if step, ok := strings.CutSuffix(key, "_failed"); ok && steps[step] && !codes[key] {
				dead = append(dead, lang+": "+key)
			}
		}
	}
	sort.Strings(missing)
	sort.Strings(dead)
	if len(missing) > 0 {
		t.Errorf("failure reasons with no text under slurmApplyReason: %v", missing)
	}
	if len(dead) > 0 {
		t.Errorf("texts for step failures no marker produces (the failure shows as unknown): %v", dead)
	}
}

// The reasons and steps the server adds itself have their text too.
func TestServerSideApplyReasonsHaveText(t *testing.T) {
	reasons, stepTexts := uiKeys(t, "slurmApplyReason"), uiKeys(t, "slurmApplyStep")
	for _, lang := range []string{"en", "zh"} {
		for _, r := range []string{messages.SlurmApplyReasonBusy, messages.SlurmApplyReasonServerUnreachable, messages.SlurmApplyReasonServerUnreachable + "_at", "unknown"} {
			if !reasons[lang][r] {
				t.Errorf("%s has no slurmApplyReason text for %s", lang, r)
			}
		}
		for _, s := range []string{messages.SlurmApplyStepCluster, messages.SlurmApplyStepAssistant, messages.SlurmApplyStepRun, messages.SlurmApplyStepGres, messages.SlurmApplyStepSetup, messages.SlurmApplyStepResume} {
			if !stepTexts[lang][s] {
				t.Errorf("%s has no slurmApplyStep text for %s", lang, s)
			}
		}
	}
}

// uiKeys reads the keys of one section of common.json in each UI language.
func uiKeys(t *testing.T, section string) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, lang := range []string{"en", "zh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "ui", "src", "i18n", "locales", lang, "common.json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		var keys map[string]string
		if err := json.Unmarshal(doc[section], &keys); err != nil {
			t.Fatalf("%s %s: %v", lang, section, err)
		}
		out[lang] = map[string]bool{}
		for k := range keys {
			out[lang][k] = true
		}
	}
	return out
}
