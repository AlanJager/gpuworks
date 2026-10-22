package runtime

import (
	"context"
	"testing"
)

func TestFirstVersion(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"NVIDIA Container Toolkit CLI version 1.20.0", "1.20.0"},
		{"ascend-docker-runtime 8.5.0.alpha1", "8.5.0.alpha1"},
		{"no version", ""},
	} {
		if got := FirstVersion(tc.raw); got != tc.want {
			t.Fatalf("FirstVersion(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

type pluginDetectingProber struct{ reported PluginInfo }

func (p pluginDetectingProber) Vendor() string { return "fake" }
func (p pluginDetectingProber) DetectRuntimePlugin(context.Context, string) PluginInfo {
	return p.reported
}

type plainProber struct{}

func (plainProber) Vendor() string { return "fake" }

func TestDetectPluginDispatchesToTheProber(t *testing.T) {
	reported := PluginInfo{Name: "Fake Runtime", Version: "1.2.3", Installed: true}
	if got := DetectPlugin(context.Background(), pluginDetectingProber{reported: reported}, "docker"); got != reported {
		t.Fatalf("DetectPlugin = %+v, want %+v", got, reported)
	}
}

func TestDetectPluginExplicitInjectionRuntimes(t *testing.T) {
	// udocker always uses explicit device injection regardless of vendor.
	for _, driver := range []string{"udocker", "", "  udocker  "} {
		if got := DetectPlugin(context.Background(), pluginDetectingProber{}, driver); got.Name != "" || got.Version != "" {
			t.Fatalf("DetectPlugin(driver=%q) = %+v, want empty", driver, got)
		}
	}
}

func TestDetectPluginWithoutVendorPlugin(t *testing.T) {
	// Hygon, PPU, and any vendor without a runtime plugin report nothing.
	if got := DetectPlugin(context.Background(), plainProber{}, "docker"); got.Name != "" || got.Installed {
		t.Fatalf("DetectPlugin(plain prober) = %+v, want empty", got)
	}
}
