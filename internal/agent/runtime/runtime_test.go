package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestPullProxyEnvIsTaskScopedAndReplacesInheritedProxyValues(t *testing.T) {
	base := []string{"PATH=/bin", "HTTP_PROXY=http://old", "https_proxy=http://old-secure", "KEEP=value"}
	if got := PullProxyEnv(context.Background(), base); len(got) != len(base) {
		t.Fatalf("plain environment changed: %v", got)
	}
	ctx := WithPullProxy(context.Background(), " http://selected.example:3128 ")
	got := PullProxyEnv(ctx, base)
	want := map[string]bool{
		"PATH=/bin": true, "KEEP=value": true,
		"HTTP_PROXY=http://selected.example:3128":  true,
		"HTTPS_PROXY=http://selected.example:3128": true,
		"http_proxy=http://selected.example:3128":  true,
		"https_proxy=http://selected.example:3128": true,
	}
	if len(got) != len(want) {
		t.Fatalf("proxy environment = %v", got)
	}
	for _, entry := range got {
		if !want[entry] {
			t.Fatalf("unexpected proxy environment entry %q in %v", entry, got)
		}
	}
}

func TestPullProxyEnvExplicitProxyOverridesBypassAndAllProxy(t *testing.T) {
	base := []string{"PATH=/bin", "ALL_PROXY=socks5://old:1080", "all_proxy=socks5://old:1080", "NO_PROXY=*", "no_proxy=*"}
	got := PullProxyEnv(WithPullProxy(context.Background(), "http://selected.example:3128"), base)
	for _, entry := range got {
		if strings.Contains(entry, "old") || strings.HasSuffix(entry, "=*") {
			t.Fatalf("explicit proxy retained a conflicting variable: %s", entry)
		}
	}
}

// An explicit proxy carries its own bypass list, replacing the ambient one,
// so a CLI pull skips the same hosts the agent's own requests do.
func TestPullProxyEnvCarriesTheProxysOwnNoProxy(t *testing.T) {
	base := []string{"PATH=/bin", "NO_PROXY=*", "no_proxy=*"}
	ctx := WithPullNoProxy(WithPullProxy(context.Background(), "http://selected.example:3128"), "registry.corp,localhost,127.0.0.1")
	got := strings.Join(PullProxyEnv(ctx, base), "\n")
	for _, want := range []string{"HTTPS_PROXY=http://selected.example:3128", "NO_PROXY=registry.corp,localhost,127.0.0.1", "no_proxy=registry.corp,localhost,127.0.0.1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("env %q lacks %s", got, want)
		}
	}
	if strings.Contains(got, "=*") {
		t.Fatalf("the ambient bypass survived: %q", got)
	}
	if route := PullRoute(ctx); route.NoProxy != "registry.corp,localhost,127.0.0.1" || route.HTTPSProxy != "http://selected.example:3128" {
		t.Fatalf("route = %+v", route)
	}
}
