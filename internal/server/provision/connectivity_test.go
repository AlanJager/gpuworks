package provision

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func row(name, status string) *CheckResult { return &CheckResult{Name: name, Status: status} }

func TestConnectivityVerdict(t *testing.T) {
	ok, bad := "ok", "warn"
	for _, tc := range []struct {
		name     string
		mode     string
		a2s, s2a *CheckResult
		want     string // "" for no row, else a substring of the detail
	}{
		{"agent_dial works", "agent_dial", row(checkServerReachability, ok), row(checkServerToAgent, bad), ""},
		{"agent_dial works, server side not probed", "agent_dial", row(checkServerReachability, ok), nil, ""},
		{"agent_dial broken, server_dial would work", "agent_dial", row(checkServerReachability, "error"), row(checkServerToAgent, ok), "choose server-initiated connection"},
		{"agent_dial broken both ways", "agent_dial", row(checkServerReachability, "error"), row(checkServerToAgent, bad), "either direction"},
		{"agent_dial broken, no address to probe", "agent_dial", row(checkServerReachability, "error"), nil, "either direction"},
		{"server_dial works", "server_dial", nil, row(checkServerToAgent, ok), ""},
		// Only the needed direction was checked; its own row says what drops it.
		{"server_dial broken, other direction unchecked", "server_dial", nil, row(checkServerToAgent, bad), ""},
		{"server_dial broken, agent_dial would work", "server_dial", row(checkServerReachability, ok), row(checkServerToAgent, bad), "choose agent-initiated connection"},
		{"nothing checked", "agent_dial", nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ConnectivityVerdict(tc.mode, tc.a2s, tc.s2a)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("verdict = %+v, want none", got)
				}
				return
			}
			if got == nil || got.Name != checkConnectivity || got.Status != "warn" || !strings.Contains(got.Detail, tc.want) {
				t.Fatalf("verdict = %+v, want a warning containing %q", got, tc.want)
			}
			if strings.Contains(tc.want, "either direction") && !strings.Contains(got.Detail, "docs/how-to/connect-nodes.md") {
				t.Fatalf("a dead end must point at the docs: %s", got.Detail)
			}
		})
	}
}

// Before the install nothing listens on the port: a refusal proves the
// route, a timeout does not.
func TestServerToAgentProbe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	if got := ServerToAgentProbe(context.Background(), "127.0.0.1", ":"+port, time.Second); got.Status != "ok" || strings.Contains(got.Detail, "nothing listens") {
		t.Fatalf("listening port = %+v", got)
	}
	_ = l.Close()
	if got := ServerToAgentProbe(context.Background(), "127.0.0.1", ":"+port, time.Second); got.Status != "ok" || !strings.Contains(got.Detail, "nothing listens there yet") {
		t.Fatalf("refused port = %+v", got)
	}
	// A security group that drops the packets: the dial times out.
	restore := dialServerToAgent
	t.Cleanup(func() { dialServerToAgent = restore })
	var dialed string
	dialServerToAgent = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = address
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
	}
	if got := ServerToAgentProbe(context.Background(), "203.0.113.1", "", 50*time.Millisecond); got.Status != "warn" || dialed != "203.0.113.1:18800" || !strings.Contains(got.Detail, "cannot reach 203.0.113.1:18800") {
		t.Fatalf("unreachable = %+v (dialed %s)", got, dialed)
	}
	if got := ServerToAgentProbe(context.Background(), "", "", time.Second); got.Status != "warn" {
		t.Fatalf("no host = %+v", got)
	}
	if got := ServerToAgentProbe(context.Background(), "10.0.0.1", "nonsense", time.Second); got.Status != "warn" || !strings.Contains(got.Detail, "invalid listen address") {
		t.Fatalf("bad listen = %+v", got)
	}
}

// With agent_dial an unreachable node is normal (NAT): the probe row appears
// only to explain a node that cannot reach the server either.
func TestAppendConnectivityShowsTheServerSideOnlyWhenItMatters(t *testing.T) {
	ctx := context.Background()
	restore := dialServerToAgent
	t.Cleanup(func() { dialServerToAgent = restore })
	dialServerToAgent = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: context.DeadlineExceeded}
	}
	healthy := []CheckResult{{Name: checkServerReachability, Status: "ok"}}
	got := AppendConnectivity(ctx, healthy, "agent_dial", "203.0.113.1", "")
	if len(got) != 1 {
		t.Fatalf("healthy agent_dial rows = %+v", got)
	}
	broken := []CheckResult{{Name: checkServerReachability, Status: "error"}}
	got = AppendConnectivity(ctx, broken, "agent_dial", "203.0.113.1", "")
	if len(got) != 3 || got[1].Name != checkServerToAgent || got[2].Name != checkConnectivity {
		t.Fatalf("broken agent_dial rows = %+v", got)
	}
	got = AppendConnectivity(ctx, nil, "server_dial", "", "")
	if len(got) != 0 {
		t.Fatalf("no address, server_dial rows = %+v", got)
	}
}

// A platform's port mapping (AutoDL, RunPod, Vast) gives an address whose
// port is not the listen port: the probe dials that address, and a
// server-dialed install it cannot reach is an error that says to open the
// port, since the agent would never be reached.
func TestServerDialProbeDialsTheMappedAddressAndFailsWhenDropped(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got := ServerToAgentProbe(context.Background(), l.Addr().String(), "0.0.0.0:6006", time.Second); got.Status != "ok" || !strings.Contains(got.Detail, l.Addr().String()) {
		t.Fatalf("mapped address = %+v", got)
	}

	restore := dialServerToAgent
	t.Cleanup(func() { dialServerToAgent = restore })
	var dialed string
	dialServerToAgent = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed = address
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: network, Err: ctx.Err()}
	}
	got := AppendConnectivity(context.Background(), nil, "server_dial", "connect.example.com:37145", "0.0.0.0:6006")
	if dialed != "connect.example.com:37145" {
		t.Fatalf("dialed %q, want the mapped address", dialed)
	}
	probe := findCheck(got, checkServerToAgent)
	if probe == nil || probe.Status != "error" || !strings.Contains(probe.Detail, "security group or firewall") || !strings.Contains(probe.Detail, "allow inbound TCP 37145") {
		t.Fatalf("server_dial rows = %+v", got)
	}
}
