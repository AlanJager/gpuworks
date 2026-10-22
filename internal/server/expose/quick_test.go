package expose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const quickFakeCloudflared = `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo 'cloudflared version 2026.9.1 (fake)'
	exit 0
fi
while true; do sleep 1; done
`

func quickEnv(t *testing.T) *ProviderEnv {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloudflared"), []byte(quickFakeCloudflared), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &ProviderEnv{Store: NewStore(t.TempDir()), Origin: "http://127.0.0.1:8800", Deps: NewDeps()}
}

func TestQuickValidate(t *testing.T) {
	if err := (quickProvider{}).Validate(&Config{Revision: "r", Provider: ProviderQuick, Desired: DesiredEnabled}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := (quickProvider{}).Validate(&Config{Revision: "r", Provider: ProviderQuick, Desired: DesiredEnabled, TokenFile: "/tmp/t"}); err == nil {
		t.Error("a quick tunnel takes no token; a configuration that carries one is a mistake worth reporting")
	}
	if err := (quickProvider{}).Validate(&Config{Revision: "r", Provider: ProviderQuick, Desired: DesiredEnabled, Hostname: "x.example.com"}); err == nil {
		t.Error("a quick tunnel cannot pin a hostname")
	}
}

func TestQuickLaunchArgs(t *testing.T) {
	env := quickEnv(t)
	cases := []struct {
		name     string
		net      Network
		contains []string
		missing  []string
	}{
		{name: "automatic", contains: []string{"tunnel", "--url", "http://127.0.0.1:8800", "--no-autoupdate"}, missing: []string{"--edge-ip-version", "--protocol"}},
		{name: "ipv4 forced", net: Network{EdgeIPVersion: "4"}, contains: []string{"--edge-ip-version", "4"}, missing: []string{"--protocol"}},
		{name: "http2", net: Network{EdgeIPVersion: "4", Protocol: "http2"}, contains: []string{"--edge-ip-version", "4", "--protocol", "http2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launch, err := (quickProvider{}).Launch(context.Background(), env,
				&Config{Revision: "r", Provider: ProviderQuick, Desired: DesiredEnabled}, tc.net)
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			joined := strings.Join(launch.Args, " ")
			for _, want := range tc.contains {
				if !strings.Contains(joined, want) {
					t.Errorf("args = %v, want %q", launch.Args, want)
				}
			}
			for _, missing := range tc.missing {
				if strings.Contains(joined, missing) {
					t.Errorf("args = %v must not contain %q", launch.Args, missing)
				}
			}
			if launch.Version != "2026.9.1" {
				t.Errorf("version = %q, want the version the binary reported", launch.Version)
			}
			if !hasEnv(launch.Env, "HOME=") {
				t.Error("the connector must run with an isolated HOME so a leftover .cloudflared/config.yaml cannot change what a quick tunnel does")
			}
		})
	}
}

// TestQuickReadyWaitsForRegistration is the bug Unsloth fixed in PR #6264 and
// the plan calls out: the URL is printed before the edge connection registers,
// and a link handed out in that window answers 530.
func TestQuickReadyWaitsForRegistration(t *testing.T) {
	env := quickEnv(t)
	cfg := &Config{Revision: "r", Provider: ProviderQuick, Desired: DesiredEnabled}

	if _, err := (quickProvider{}).Ready(context.Background(), env, cfg, StreamState{URL: "https://x.trycloudflare.com"}); err == nil {
		t.Fatal("a URL without a registered connection is not ready")
	}
	if _, err := (quickProvider{}).Ready(context.Background(), env, cfg, StreamState{
		Registration: &Registration{Edge: "lax01", Protocol: "quic"},
	}); err == nil {
		t.Fatal("a registration without an address is not ready")
	}
	ready, err := (quickProvider{}).Ready(context.Background(), env, cfg, StreamState{
		URL:          "https://x.trycloudflare.com",
		Registration: &Registration{Edge: "lax01", Protocol: "quic"},
	})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready.State != StateReady || ready.URL != "https://x.trycloudflare.com" {
		t.Fatalf("ready = %+v", ready)
	}
	// Every ready quick address carries its limits: temporary hostname, no SSE.
	for _, want := range []string{"temporary", "SSE"} {
		if !strings.Contains(ready.Message, want) {
			t.Errorf("message %q should mention %q", ready.Message, want)
		}
	}
}
