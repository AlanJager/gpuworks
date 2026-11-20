package conformance_test

import (
	"bufio"
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/conformance"
)

func TestMain(m *testing.M) {
	if os.Getenv(envPlugin) == "1" {
		runTestPlugin()
		return
	}
	os.Exit(m.Run())
}

// The child test runs the suite against the test plugin; envChild holds
// the plugin's faults, envLifecycle its package's lifecycle.
const (
	envChild     = "CONFORMANCE_TEST_CHILD"
	envLifecycle = "CONFORMANCE_TEST_LIFECYCLE"
)

// More faults, each breaking one rule:
const (
	// modeNoPush pushes nothing.
	modeNoPush = "no-push"
	// modePushOnlyFresh pushes only to a server that holds nothing.
	modePushOnlyFresh = "push-only-when-nothing-held"
	// modeWrongVersion answers initialize with another version.
	modeWrongVersion = "wrong-version"
	// modeRefuseAll refuses every route request as if its principal were
	// invalid.
	modeRefuseAll = "refuse-all-routes"
)

// TestEachFaultFailsOnlyItsRule runs the suite against the test plugin,
// without faults and with each fault, and checks which rules pass, fail
// and are skipped: a correct plugin passes every rule that applies, and
// each fault fails exactly the rules it breaks.
func TestEachFaultFailsOnlyItsRule(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run on linux and darwin")
	}
	resident := func(fail ...string) map[string]string {
		out := outcomes("PASS")
		out[conformance.RuleOnDemandDoesNotPush] = "SKIP"
		for _, r := range fail {
			out[r] = "FAIL"
		}
		return out
	}
	onDemand := func(onDemandRule string) map[string]string {
		out := outcomes("PASS")
		out[conformance.RuleUnchangedPushSkipped] = "SKIP"
		out[conformance.RuleChangedPushApplied] = "SKIP"
		out[conformance.RuleOnDemandDoesNotPush] = onDemandRule
		return out
	}
	for _, c := range []struct {
		mode, lifecycle string
		want            map[string]string
	}{
		{"", extapi.LifecycleResident, resident()},
		{modeNoPush, extapi.LifecycleOnDemand, onDemand("PASS")},
		{"", extapi.LifecycleOnDemand, onDemand("FAIL")},
		{modeWrongVersion, extapi.LifecycleResident, resident(conformance.RuleInitialize)},
		{modeUndeclaredCapability, extapi.LifecycleResident, resident(conformance.RuleDeclaredCapabilities)},
		{modeUndeclaredCall, extapi.LifecycleResident, resident(conformance.RuleDeclaredCapabilities)},
		{modeHangHealth, extapi.LifecycleResident, resident(conformance.RuleHealth)},
		{modeLinger, extapi.LifecycleResident, resident(conformance.RuleShutdown)},
		{modeAlwaysPush, extapi.LifecycleResident, resident(conformance.RuleUnchangedPushSkipped)},
		{modePushOnlyFresh, extapi.LifecycleResident, resident(conformance.RuleChangedPushApplied)},
		{modeRefuseAll, extapi.LifecycleResident, resident(conformance.RuleRouteServesValidPrincipal)},
		{modeIgnoreAction, extapi.LifecycleResident, resident(conformance.RuleRouteRefusesOtherAction)},
		{modeActionInDeclaredSet, extapi.LifecycleResident, resident(conformance.RuleRouteRefusesOtherAction)},
		{modeSkipPrincipal, extapi.LifecycleResident, resident(conformance.RuleRouteRefusesMissingPrincipal,
			conformance.RuleRouteRefusesWrongMAC, conformance.RuleRouteRefusesOtherAction)},
	} {
		name := c.lifecycle + "/" + c.mode
		if c.mode == "" {
			name = c.lifecycle + "/correct"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, out := runChild(t, c.mode, c.lifecycle)
			if !maps.Equal(got, c.want) {
				t.Errorf("rule outcomes\n got %v\nwant %v\nchild output:\n%s", got, c.want, out)
			}
		})
	}
}

func outcomes(v string) map[string]string {
	out := map[string]string{}
	for _, r := range conformance.Rules() {
		out[r] = v
	}
	return out
}

// subtestLine is a rule's outcome in the child's verbose output.
var subtestLine = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): TestRunChild/([a-z_]+) `)

// runChild runs TestRunChild in a new copy of the test binary and returns
// each rule's outcome, and the child's output.
func runChild(t *testing.T, mode, lifecycle string) (map[string]string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestRunChild$", "-test.v", "-test.count=1", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), envChild+"="+mode, envLifecycle+"="+lifecycle)
	out, _ := cmd.CombinedOutput() // a failing rule fails the child
	got := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if m := subtestLine.FindStringSubmatch(sc.Text()); m != nil {
			got[m[2]] = m[1]
		}
	}
	return got, string(out)
}

// TestRunChild is the suite run that TestEachFaultFailsOnlyItsRule
// starts in a child process.
func TestRunChild(t *testing.T) {
	mode, ok := os.LookupEnv(envChild)
	if !ok {
		t.Skip("run by TestEachFaultFailsOnlyItsRule in a child process")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := writeManifest(t, t.TempDir(), os.Getenv(envLifecycle))
	conformance.Run(t, conformance.Config{
		Binary: exe, Manifest: manifest,
		Env:           []string{envPlugin + "=1", envManifest + "=" + manifest, envMode + "=" + mode},
		InitTimeout:   10 * time.Second,
		HealthTimeout: time.Second, ShutdownTimeout: time.Second, PushWait: 500 * time.Millisecond,
	})
}

// testManifest is the test plugin's package: it requires table.banners and
// routes, and declares an administrator route with a parameter, a gated
// route and a public route. An on_demand package neither asks to push nor
// declares a feature, as the server's policy demands, so its second route
// is ungated.
func testManifest(lifecycle string) *extapi.Manifest {
	m := &extapi.Manifest{
		Schema: extapi.ManifestSchema, ID: "acme_conformance", Publisher: "acme", Version: "1.0.0",
		Created:     extapi.NewTime(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)),
		Protocol:    extapi.Range{Min: 1, Max: 1},
		Requires:    map[string]int{extapi.CapRoutes: 1, extapi.TableCapability(extapi.KindBanners): 1},
		Permissions: []string{"http:public", "http:routes", "push:banners"},
		Features: []extapi.FeatureDecl{{Key: "acme.conformance", Presentation: extapi.PresentationPromote,
			Gate: extapi.GateSelf}},
		Routes: []extapi.RouteDecl{
			{Method: "GET", Pattern: "/jobs/{id}", Action: "acme_conformance.jobs_read", Access: extapi.RouteAdmin, Ungated: true},
			{Method: "POST", Pattern: "/jobs", Action: "acme_conformance.jobs_create", Access: extapi.RouteAdmin, Feature: "acme.conformance"},
			{Method: "GET", Pattern: "/", Action: "acme_conformance.home", Access: extapi.RoutePublic, Ungated: true},
		},
		Runtime: &extapi.RuntimeDecl{Lifecycle: lifecycle},
		Files: []extapi.FileEntry{{Path: "bin/linux_amd64/acme-conformance", Exec: true, Size: 4096,
			SHA256: "51a1f05af85e342e3c849b47d387086476282d5f50dc240c19216d6edfb1eb5a"}},
	}
	if lifecycle == extapi.LifecycleOnDemand {
		m.Permissions = m.Permissions[:2]
		m.Features = nil
		m.Routes[1].Feature, m.Routes[1].Ungated = "", true
	}
	return m
}

// writeManifest writes the test plugin's manifest into dir and returns its
// path.
func writeManifest(t *testing.T, dir, lifecycle string) string {
	t.Helper()
	raw, err := extapi.Marshal(testManifest(lifecycle))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extapi.DecodeManifest(raw); err != nil {
		t.Fatalf("the test manifest does not decode: %v", err)
	}
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
