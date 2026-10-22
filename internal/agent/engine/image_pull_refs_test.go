package engine

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// networkConfigHolder mirrors the node runtime: it owns the network config and
// hands the engine a reader, so a replacement is visible without a second copy.
func networkConfigHolder() (func(proto.NetworkConfig), EngineOption) {
	var mu sync.Mutex
	var cfg proto.NetworkConfig
	set := func(next proto.NetworkConfig) {
		mu.Lock()
		cfg = next
		mu.Unlock()
	}
	read := func() proto.NetworkConfig {
		mu.Lock()
		defer mu.Unlock()
		return cfg
	}
	return set, WithNetworkConfig(read)
}

func TestImagePullCandidatesKeepTheOlderServersOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec proto.InstanceDesiredState
		want []string
	}{
		{
			name: "the server order is kept verbatim",
			spec: proto.InstanceDesiredState{Image: "repo:v1", ImagePullRefs: []string{"mirror-1/repo:v1", "mirror-2/repo:v1", "repo:v1"}},
			want: []string{"mirror-1/repo:v1", "mirror-2/repo:v1", "repo:v1"},
		},
		{
			name: "a repeated candidate is tried once",
			spec: proto.InstanceDesiredState{Image: "repo:v1", ImagePullRefs: []string{"mirror-1/repo:v1", " mirror-1/repo:v1 ", "repo:v1"}},
			want: []string{"mirror-1/repo:v1", "repo:v1"},
		},
		{
			name: "blank candidates are dropped",
			spec: proto.InstanceDesiredState{Image: "repo:v1", ImagePullRefs: []string{"  ", "mirror-1/repo:v1", ""}},
			want: []string{"mirror-1/repo:v1"},
		},
		{
			name: "an empty candidate list falls back to the single pull ref",
			spec: proto.InstanceDesiredState{Image: "repo:v1", ImagePullRef: "mirror-1/repo:v1"},
			want: []string{"mirror-1/repo:v1"},
		},
		{
			name: "with neither list nor pull ref the canonical image is used",
			spec: proto.InstanceDesiredState{Image: "repo:v1"},
			want: []string{"repo:v1"},
		},
		{
			name: "a spec with no image at all yields no candidates",
			spec: proto.InstanceDesiredState{},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := imagePullCandidates(tc.spec)
			if len(got) != len(tc.want) {
				t.Fatalf("candidates = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("candidates = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// An older server's flat order reaches the driver as it is, and a failing
// reference falls back to the next.
func TestStartOrAdoptPullsTheRefsInOrderAndFallsBackAcrossThem(t *testing.T) {
	h := newSleepHTTPServer(t)
	driver := &imageToleranceDriver{
		exists: []bool{false, false},
		pullErrs: map[string]error{
			"one.example/repo:v1": errors.New("mirror unavailable"),
			"two.example/repo:v1": nil,
		},
	}
	e := NewWithProber(driver, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{
		UUID: "image-order", Image: "repo:v1",
		ImagePullRefs: []string{"one.example/repo:v1", "two.example/repo:v1"},
		APIPort:       h.port(t), Ports: []int{h.port(t)}, StartTimeout: 1,
	})
	if len(driver.pullRefs) != 2 || driver.pullRefs[0] != "one.example/repo:v1" || driver.pullRefs[1] != "two.example/repo:v1" {
		t.Fatalf("pull refs = %v", driver.pullRefs)
	}
	if driver.runCalls != 1 {
		t.Fatalf("run calls = %d, want 1", driver.runCalls)
	}
}

// When every candidate fails the error must name each ref actually attempted,
// so an operator can tell which hosts were tried.
func TestStartOrAdoptFailureNamesEveryAttemptedRef(t *testing.T) {
	h := newSleepHTTPServer(t)
	driver := &imageToleranceDriver{
		exists: []bool{false, false, false, false},
		pullErrs: map[string]error{
			"one.example/repo:v1": errors.New("first mirror down"),
			"two.example/repo:v1": errors.New("second mirror down"),
		},
	}
	reported := make(chan proto.InstanceStatus, 32)
	e := NewWithProber(driver, func(st proto.InstanceStatus) { reported <- st }, slog.Default(), t.TempDir(), injectionProber{})
	e.startOrAdopt(context.Background(), proto.InstanceDesiredState{
		UUID: "image-order-fail", Image: "repo:v1",
		ImagePullRefs: []string{"one.example/repo:v1", "two.example/repo:v1"},
		APIPort:       h.port(t), Ports: []int{h.port(t)}, StartTimeout: 1,
	})
	var failure string
	for {
		select {
		case st := <-reported:
			if st.State == proto.InstFailed {
				failure = st.Message
			}
		default:
			if failure == "" {
				t.Fatal("no failed status was reported")
			}
			for _, ref := range []string{"one.example/repo:v1", "two.example/repo:v1"} {
				if !strings.Contains(failure, ref) {
					t.Fatalf("failure %q does not name attempted ref %s", failure, ref)
				}
			}
			return
		}
	}
}

// The engine does not own the network configuration: it reads the node's
// through the injected reader, so a replacement on the node is visible
// immediately and an engine without a reader is simply the zero config.
func TestEngineNetworkFollowsTheInjectedReader(t *testing.T) {
	setNetwork, networkOption := networkConfigHolder()
	e := NewWithProber(&imageToleranceDriver{}, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir(), injectionProber{}, networkOption)
	if got := e.network(); len(got.Rewrites) != 0 || got.HTTPProxy != "" {
		t.Fatalf("default network = %#v", got)
	}
	setNetwork(proto.NetworkConfig{HTTPProxy: "http://proxy.example:3128"})
	if got := e.network(); got.HTTPProxy != "http://proxy.example:3128" {
		t.Fatalf("network after set = %#v", got)
	}
	setNetwork(proto.NetworkConfig{})
	if got := e.network(); got.HTTPProxy != "" {
		t.Fatalf("network was not replaced: %#v", got)
	}
	bare := NewWithProber(&imageToleranceDriver{}, func(proto.InstanceStatus) {}, slog.Default(), t.TempDir(), injectionProber{})
	if got := bare.network(); got.HTTPProxy != "" || len(got.Rewrites) != 0 {
		t.Fatalf("engine without a reader = %#v", got)
	}
}
