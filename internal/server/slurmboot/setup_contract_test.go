package slurmboot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

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
		res, ferr = w.parsed(t, provider.SlurmModeSetup)
		if ferr != nil || res.Done != "ok" || len(res.Steps) == 0 || !res.Shared {
			t.Fatalf("setup: %+v %v", res, ferr)
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
	keys := applyReasonKeys(t)
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

// Every failure reason the scripts can produce has text in both languages:
// each precheck code (pre_fail <code>, reason=<code>) and "<step>_failed" for
// each step that marks itself failed. The precheck's own step and the script
// directory's (always followed by pre_fail script_dir_unsafe) carry a code
// instead; done, distribute and server_reach are not failures of the run
// (provider.ParseSlurmApply passes over them).
func TestEveryScriptFailureHasText(t *testing.T) {
	codes := map[string]bool{}
	preFail := regexp.MustCompile(`pre_fail ([a-z_]+) "`)
	precheck := regexp.MustCompile(`step=precheck status=failed reason=([a-z_]+)`)
	stepFailed := regexp.MustCompile(`step=([a-z_]+) status=failed`)
	for _, s := range []string{setupTemplate, setupConfPart, setupNodesPart, setupReachPart, setupCheckPart, setupDirPart} {
		for _, m := range preFail.FindAllStringSubmatch(s, -1) {
			codes[m[1]] = true
		}
		for _, m := range precheck.FindAllStringSubmatch(s, -1) {
			codes[m[1]] = true
		}
		for _, m := range stepFailed.FindAllStringSubmatch(s, -1) {
			switch m[1] {
			case "precheck", "script_dir", "done", "distribute", "server_reach":
			default:
				codes[m[1]+"_failed"] = true
			}
		}
	}
	if len(codes) < 10 {
		t.Fatalf("found only %d failure codes in the scripts: the patterns no longer match", len(codes))
	}
	var missing []string
	for lang, k := range applyReasonKeys(t) {
		for c := range codes {
			if !k[c] {
				missing = append(missing, lang+": "+c)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("failure reasons with no text under slurmApplyReason: %v", missing)
	}
}

// applyReasonKeys reads the slurmApplyReason keys of each UI language.
func applyReasonKeys(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for _, lang := range []string{"en", "zh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "ui", "src", "i18n", "locales", lang, "common.json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Reasons map[string]string `json:"slurmApplyReason"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		out[lang] = map[string]bool{}
		for k := range doc.Reasons {
			out[lang][k] = true
		}
	}
	return out
}
