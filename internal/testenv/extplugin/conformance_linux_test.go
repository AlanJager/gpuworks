//go:build linux

package extplugin

import (
	"bufio"
	"bytes"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/conformance"
)

// A conformance child runs the suite against the fake plugin laid out in
// envConformancePackage, started with the flags in envConformanceArgs (one
// per line).
const (
	envConformancePackage = "EXTPLUGIN_CONFORMANCE_PACKAGE"
	envConformanceArgs    = "EXTPLUGIN_CONFORMANCE_ARGS"
)

// conformancePush is the banners table the fake plugin pushes at start.
const conformancePush = `banners={"banners":[{"id":"maintenance","audience":"admin","steps":[{"severity":"info","notice":{"code":"ACME_NOTICE"}}]}]}`

// TestFakePluginPassesConformance runs pkg/pluginsdk/conformance against
// the in-repo fake plugin, which proves the suite against a known-good
// binary, and against the fake plugin's broken modes, each of which must
// fail exactly the rules it breaks. Each run is a child process, so a
// failing rule is observed rather than failing this test.
//
//	go test -run TestFakePluginPassesConformance ./internal/testenv/extplugin/
func TestFakePluginPassesConformance(t *testing.T) {
	pkg := conformancePackage(t)
	all := func(fail ...string) map[string]string {
		out := map[string]string{}
		for _, r := range conformance.Rules() {
			out[r] = "PASS"
		}
		out[conformance.RuleOnDemandDoesNotPush] = "SKIP"
		for _, r := range fail {
			out[r] = "FAIL"
		}
		return out
	}
	for _, c := range []struct {
		name  string
		flags []string
		want  map[string]string
	}{
		{"correct", nil, all()},
		{"skip_principal_check", []string{"--skip-principal-check"}, all(conformance.RuleRouteRefusesMissingPrincipal,
			conformance.RuleRouteRefusesWrongMAC, conformance.RuleRouteRefusesOtherAction)},
		{"ignore_route_action", []string{"--ignore-route-action"}, all(conformance.RuleRouteRefusesOtherAction)},
		{"hang_health", []string{"--hang", "health"}, all(conformance.RuleHealth)},
		{"undeclared_capability", []string{"--requires", "table.banners@1-1,host.nodes@1-1"},
			all(conformance.RuleDeclaredCapabilities)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, out := runConformanceChild(t, pkg, c.flags)
			if !maps.Equal(got, c.want) {
				t.Errorf("rule outcomes\n got %v\nwant %v\nchild output:\n%s", got, c.want, out)
			}
		})
	}
}

// conformancePackage lays the fake plugin out as an installed package: its
// manifest, and the binary below it, where the fake plugin finds the
// manifest's id, version, digest and routes.
func conformancePackage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	m := &extapi.Manifest{
		Schema: extapi.ManifestSchema, ID: "acme_fake", Publisher: "acme", Version: "1.0.0",
		Created:  extapi.NewTime(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)),
		Protocol: extapi.Range{Min: 1, Max: 1},
		Requires: map[string]int{extapi.CapRoutes: 1, extapi.TableCapability(extapi.KindBanners): 1},
		Permissions: []string{"http:public", "http:routes", "push:banners"},
		Features: []extapi.FeatureDecl{{Key: "acme.tools", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf}},
		Routes: []extapi.RouteDecl{
			{Method: "GET", Pattern: "/jobs/{id}", Action: "acme_fake.jobs_read", Access: extapi.RouteAdmin, Ungated: true},
			{Method: "POST", Pattern: "/jobs", Action: "acme_fake.jobs_create", Access: extapi.RouteAdmin, Feature: "acme.tools"},
			{Method: "GET", Pattern: "/", Action: "acme_fake.home", Access: extapi.RoutePublic, Ungated: true},
		},
		Runtime: &extapi.RuntimeDecl{Lifecycle: extapi.LifecycleResident},
		Files: []extapi.FileEntry{{Path: "bin/fakeplugin", Exec: true, Size: 1,
			SHA256: "51a1f05af85e342e3c849b47d387086476282d5f50dc240c19216d6edfb1eb5a"}},
	}
	raw, err := extapi.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// A copy, not a link: the fake plugin looks for the manifest above
	// its resolved executable.
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(fakeBin)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, "bin", "fakeplugin"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// conformanceLine is a rule's outcome in a child's verbose output.
var conformanceLine = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): TestConformanceChild/([a-z_]+) `)

// runConformanceChild runs TestConformanceChild in a new copy of the test
// binary and returns each rule's outcome, and the child's output.
func runConformanceChild(t *testing.T, pkg string, flags []string) (map[string]string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestConformanceChild$", "-test.v", "-test.count=1", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), envConformancePackage+"="+pkg, envConformanceArgs+"="+strings.Join(flags, "\n"))
	out, _ := cmd.CombinedOutput() // a failing rule fails the child
	got := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if m := conformanceLine.FindStringSubmatch(sc.Text()); m != nil {
			got[m[2]] = m[1]
		}
	}
	return got, string(out)
}

// TestConformanceChild is the suite run TestFakePluginPassesConformance
// starts in a child process.
func TestConformanceChild(t *testing.T) {
	pkg, ok := os.LookupEnv(envConformancePackage)
	if !ok {
		t.Skip("run by TestFakePluginPassesConformance in a child process")
	}
	args := []string{"serve", "--routes", "--push", conformancePush}
	if extra := os.Getenv(envConformanceArgs); extra != "" {
		args = append(args, strings.Split(extra, "\n")...)
	}
	conformance.Run(t, conformance.Config{
		Binary: filepath.Join(pkg, "bin", "fakeplugin"), Manifest: filepath.Join(pkg, "manifest.json"), Args: args,
		HealthTimeout: time.Second, ShutdownTimeout: 5 * time.Second, PushWait: 500 * time.Millisecond,
	})
}
