package expose

import (
	"bytes"
	"strings"
	"testing"
)

// cloudflared prints the configuration it applies, and that body is the ingress
// rules: the hostnames this server is published under and the service they
// point at. The plan requires the configuration body never to reach the log.
func TestConnectorOutputRedactsTheConfigurationBody(t *testing.T) {
	const line = `2026-09-22T09:00:00Z INF Updated to new configuration config="{\"ingress\":[{\"hostname\":\"gpu.example.com\",\"service\":\"http://127.0.0.1:8800\"}]}" version=3` + "\n"

	var log bytes.Buffer
	out := NewConnectorOutput(&log)
	if _, err := out.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	got := log.String()
	for _, secret := range []string{"gpu.example.com", "127.0.0.1:8800", "ingress"} {
		if strings.Contains(got, secret) {
			t.Errorf("the configuration body reached the log (%q in %q)", secret, got)
		}
	}
	if !strings.Contains(got, "config=[redacted]") {
		t.Errorf("the line should still say a configuration arrived: %q", got)
	}
	if !strings.Contains(got, "version=3") {
		t.Errorf("what follows the body must survive: %q", got)
	}
	// Nothing that was not a configuration body is touched.
	var plain bytes.Buffer
	out = NewConnectorOutput(&plain)
	if _, err := out.Write([]byte("INF |  https://abc.trycloudflare.com  |\nINF Registered tunnel connection connIndex=0 ip=198.41.192.1 location=lax99 protocol=quic\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain.String(), "https://abc.trycloudflare.com") || !strings.Contains(plain.String(), "Registered tunnel connection") {
		t.Errorf("ordinary lines must pass through unchanged: %q", plain.String())
	}
	if out.Snapshot().Registration == nil {
		t.Error("the registration must still be read from the redacted stream")
	}
}

// The body is also kept out of what the supervisor stores: a status message
// built from a trouble line must not carry the ingress rules either.
func TestConfigBodyStaysOutOfTheObservation(t *testing.T) {
	var log bytes.Buffer
	out := NewConnectorOutput(&log)
	body := `2026-09-22T09:00:00Z WRN Failed to apply configuration config="{\"ingress\":[{\"hostname\":\"secret.example.com\"}]}"` + "\n"
	if _, err := out.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	state := out.Snapshot()
	for _, line := range state.Trouble {
		if strings.Contains(line, "secret.example.com") {
			t.Fatalf("a trouble line carried the configuration body: %q", line)
		}
	}
	if hint := hintFor(state.Trouble); strings.Contains(hint, "secret.example.com") {
		t.Fatalf("the hint carried the configuration body: %q", hint)
	}
}
