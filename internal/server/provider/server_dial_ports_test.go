package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestChooseMappedPortTakesAFreeMappedPort(t *testing.T) {
	// The mappings and listeners of a Vast instance (vastai/base-image,
	// 2026-10-19): Caddy, Jupyter, Syncthing and sshd hold the rest.
	mapped := map[int]int{22: 18607, 1111: 18222, 6006: 18335, 8080: 18847, 8384: 18168, 10100: 18211, 10200: 18685, 18204: 18204}
	listening := map[int]bool{22: true, 1111: true, 6006: true, 8080: true, 8384: true, 18204: true, 11111: true}
	for _, tc := range []struct {
		preferred, want int
		ok              bool
	}{
		{0, 10100, true},
		{18800, 10100, true}, // the default listen port is not mapped here
		{10200, 10200, true},
		{6006, 10100, true}, // held by Caddy
		{22, 10100, true},
	} {
		got, ok := ChooseMappedPort(mapped, listening, tc.preferred)
		if got != tc.want || ok != tc.ok {
			t.Errorf("preferred %d: got %d %v, want %d %v", tc.preferred, got, ok, tc.want, tc.ok)
		}
	}
	listening[10100], listening[10200] = true, true
	if got, ok := ChooseMappedPort(mapped, listening, 0); ok {
		t.Fatalf("every mapped port is held, yet got %d", got)
	}
	if got, ok := ChooseMappedPort(map[int]int{22: 10033}, nil, 22); ok {
		t.Fatalf("SSH's port was given to the agent: %d", got)
	}
}

func TestInstanceRestartErrorMatchesTheSentinel(t *testing.T) {
	err := fmt.Errorf("runpod: %w", &InstanceRestartError{Reason: "RunPod opens ports by changing the pod"})
	if !errors.Is(err, ErrInstanceRestartRequired) {
		t.Fatalf("%v does not match ErrInstanceRestartRequired", err)
	}
	if PortOf("0.0.0.0:6006") != 6006 || PortOf(":18800") != 18800 || PortOf("bad") != 0 {
		t.Fatal("PortOf")
	}
}
