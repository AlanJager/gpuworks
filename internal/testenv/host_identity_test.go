package testenv

import (
	"testing"
	"time"
)

// A hermetic node's agent runs in the test process, so the firmware UUID it
// could read is the test machine's: the same for every node, and readable
// only where the tests run as root. A node reports none unless the test gives
// it one, so what a test sees does not depend on the account it runs under.
func TestHermeticNodesReportOnlyTheHostSystemUUIDTheyAreGiven(t *testing.T) {
	e := New(t)
	plain := e.AddNode("plain", WithGPUs(1, GPUSpec{Model: "H800"}))
	given := e.AddNode("given", WithGPUs(1, GPUSpec{Model: "H800"}), WithHostSystemUUID("4C4C4544-0042-3510-8051-B4C04F4E3432"))
	e.WaitNodeGPUs(t, "plain", 1)
	e.WaitNodeGPUs(t, "given", 1)
	// The hello that carries the UUID precedes the card report waited for
	// above; the store write it causes is polled for briefly all the same.
	deadline := time.Now().Add(5 * time.Second)
	for {
		node, err := e.Store().NodeByUUID(given.UUID)
		if err != nil {
			t.Fatal(err)
		}
		if node.HostSystemUUID == "4c4c4544-0042-3510-8051-b4c04f4e3432" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the given node's host system uuid = %q", node.HostSystemUUID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	node, err := e.Store().NodeByUUID(plain.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if node.HostSystemUUID != "" {
		t.Fatalf("a hermetic node reported the test machine's host system uuid %q", node.HostSystemUUID)
	}
}
