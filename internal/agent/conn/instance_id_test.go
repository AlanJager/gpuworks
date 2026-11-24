package conn

import (
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
	stopAfterHello(stop)
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
		stopAfterHello(stop)
		hostSystemUUID = prev
	}
}

// A hermetic agent's own source replaces the machine's firmware UUID in the
// hello, also when it has none to report: several such agents share one test
// machine, whose UUID would otherwise be every node's.
func TestHelloCarriesTheCredentialsHostSystemUUID(t *testing.T) {
	prev := hostSystemUUID
	hostSystemUUID = func() string { return "4c4c4544-0042-3510-8051-b4c04f4e3432" }
	defer func() { hostSystemUUID = prev }()
	for _, want := range []string{"0f0e0d0c-0b0a-0908-0706-050403020100", ""} {
		s := &laneTestServer{sessionID: "sess-1"}
		_, stop := runAgainstWith(t, s, false, func(c *Conn) {
			c.creds.HostSystemUUID = func() string { return want }
		})
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
		stopAfterHello(stop)
	}
}

// stopAfterHello ends an agent whose hello the server has just recorded. The
// agent is then still inside its handshake, and a Run cancelled there reports
// the cancellation or the connection torn down under it, whichever it meets
// first; neither is what these tests are about. stop itself fails the test
// when Run does not return.
func stopAfterHello(stop func() error) { _ = stop() }
