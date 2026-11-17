package conn

import (
	"context"
	"encoding/hex"
	"testing"
	"time"
)

// The agent names its process in every hello, the same id for every
// connection the process makes, so the server can tell two processes
// trading one node's connection from one process reconnecting.
func TestHelloCarriesTheProcessInstanceID(t *testing.T) {
	id := processInstanceID()
	if raw, err := hex.DecodeString(id); err != nil || len(raw) != 16 {
		t.Fatalf("instance id %q is not 16 random bytes in hex", id)
	}
	if again := processInstanceID(); again != id {
		t.Fatalf("instance id changed within one process: %q then %q", id, again)
	}
	s := &laneTestServer{sessionID: "sess-1"}
	_, stop := runAgainst(t, s, false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.agentHellos)
		var got string
		if n > 0 {
			got = s.agentHellos[0].InstanceID
		}
		s.mu.Unlock()
		if n > 0 {
			if got != id {
				t.Fatalf("hello instance_id = %q, want %q", got, id)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no hello reached the server")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stop(); err != nil && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
}

// The hello reports the machine's firmware UUID as the reader gives it, and
// sends none when the reader cannot tell (the key is then omitted).
func TestHelloCarriesTheHostSystemUUID(t *testing.T) {
	for _, want := range []string{"4c4c4544-0042-3510-8051-b4c04f4e3432", ""} {
		prev := hostSystemUUID
		hostSystemUUID = func() string { return want }
		s := &laneTestServer{sessionID: "sess-1"}
		_, stop := runAgainst(t, s, false)
		deadline := time.Now().Add(5 * time.Second)
		for {
			s.mu.Lock()
			n := len(s.agentHellos)
			var got string
			if n > 0 {
				got = s.agentHellos[0].HostSystemUUID
			}
			s.mu.Unlock()
			if n > 0 {
				if got != want {
					t.Fatalf("hello host_system_uuid = %q, want %q", got, want)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no hello reached the server")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := stop(); err != nil && err != context.Canceled {
			t.Fatalf("Run: %v", err)
		}
		hostSystemUUID = prev
	}
}
