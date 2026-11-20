package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func checkPorts(t *testing.T, host string) []int {
	t.Helper()
	// Reserve all four ports simultaneously, including the derived adjacent
	// member-session port, before letting the tested helper bind them.
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	var database, session net.Listener
	for range 100 {
		var err error
		database, err = net.Listen("tcp4", net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatal(err)
		}
		port := database.Addr().(*net.TCPAddr).Port
		if port == 65535 {
			database.Close()
			continue
		}
		session, err = net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port+1)))
		if err == nil {
			break
		}
		database.Close()
	}
	if session == nil {
		t.Fatal("could not reserve adjacent database/member-session ports")
	}
	listeners = append(listeners, database, session)
	var ports []int
	for range 2 {
		l, err := net.Listen("tcp4", net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return append(ports, database.Addr().(*net.TCPAddr).Port, session.Addr().(*net.TCPAddr).Port)
}

func TestClusterCheckAuthenticatedProbeAndCancellation(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	token := strings.Repeat("a", 64)
	go func() { clusterCheckServe(ctx, listener, token); close(done) }()
	defer func() { cancel(); listener.Close(); <-done }()
	port := listener.Addr().(*net.TCPAddr).Port
	if err := clusterCheckProbe(ctx, "127.0.0.1", port, token); err != nil {
		t.Fatal(err)
	}
	if err := clusterCheckProbe(ctx, "127.0.0.1", port, strings.Repeat("b", 64)); err == nil {
		t.Fatal("foreign challenge satisfied check")
	}
	cancel()
	if err := clusterCheckProbe(ctx, "127.0.0.1", port, token); err == nil {
		t.Fatal("canceled check succeeded")
	}
}

func TestClusterCheckRejectsUnrelatedListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buffer := make([]byte, 65)
		io.ReadFull(c, buffer)
		io.WriteString(c, "foreign-service\n")
	}()
	err = clusterCheckProbe(t.Context(), "127.0.0.1", listener.Addr().(*net.TCPAddr).Port, strings.Repeat("a", 64))
	<-done
	if err == nil || !strings.Contains(err.Error(), "did not authenticate") {
		t.Fatalf("accepted unrelated service: %v", err)
	}
}

func TestClusterCheckNodeProtocolAndListenerCleanup(t *testing.T) {
	ports := checkPorts(t, "127.0.0.1")
	token := strings.Repeat("a", 64)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	requests, writer := io.Pipe()
	reader, replies := io.Pipe()
	defer requests.Close()
	defer writer.Close()
	defer reader.Close()
	defer replies.Close()
	done := make(chan error, 1)
	go func() {
		done <- clusterCheckNodeRun(ctx, requests, replies, func(context.Context, clusterCheckRequest) error { return nil })
	}()
	encoder, decoder := json.NewEncoder(writer), json.NewDecoder(reader)
	exchange := func(req clusterCheckRequest) clusterCheckResponse {
		t.Helper()
		if err := encoder.Encode(req); err != nil {
			t.Fatal(err)
		}
		var response clusterCheckResponse
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := exchange(clusterCheckRequest{Action: "prepare", Host: "127.0.0.1", HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Token: token, Targets: map[string]string{"127.0.0.1": token}}); response.Error != "" {
		t.Fatal(response.Error)
	}
	for _, port := range ports {
		if err := clusterCheckProbe(ctx, "127.0.0.1", port, token); err != nil {
			t.Fatal(err)
		}
	}
	if response := exchange(clusterCheckRequest{Action: "probe", Target: "127.0.0.1", Port: ports[1], Token: "foreign"}); response.Error == "" {
		t.Fatal("accepted wrong command token")
	}
	if response := exchange(clusterCheckRequest{Action: "probe", Target: "127.0.0.1", Port: ports[0], Token: token, Targets: map[string]string{"127.0.0.1": token}}); response.Error == "" {
		t.Fatal("accepted unprepared probe port")
	}
	for _, port := range ports[1:] {
		if response := exchange(clusterCheckRequest{Action: "probe", Target: "127.0.0.1", Port: port, Token: token}); response.Error != "" {
			t.Fatal(response.Error)
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("helper ignored cancellation")
	}
	for _, port := range ports {
		l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("listener leaked: %v", err)
		}
		l.Close()
	}
}

type fakeClusterCheckSession struct {
	host       string
	port       int
	requests   []clusterCheckRequest
	listener   net.Listener
	done       chan struct{}
	closed     bool
	fail       string
	closeError error
}

func (s *fakeClusterCheckSession) Exchange(req clusterCheckRequest) (clusterCheckResponse, error) {
	s.requests = append(s.requests, req)
	if req.Action == "prepare" {
		listener, err := net.Listen("tcp4", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
		if err != nil {
			return clusterCheckResponse{}, err
		}
		s.listener = listener
		s.done = make(chan struct{})
		go func() { clusterCheckServe(context.Background(), listener, req.Token); close(s.done) }()
		checks := map[string]string{"runtime": "", "account": "", "directory": ""}
		if !req.NoService {
			checks["service"] = ""
		}
		for _, port := range []int{req.HTTP, req.Enrollment, req.Database, req.Database + 1} {
			checks["port:"+strconv.Itoa(port)] = ""
		}
		return clusterCheckResponse{Checks: checks}, nil
	}
	if req.Action == "probe" && s.fail == req.Target {
		return clusterCheckResponse{Error: "injected firewall failure"}, nil
	}
	return clusterCheckResponse{}, nil
}
func (s *fakeClusterCheckSession) Close() error {
	s.closed = true
	if s.listener != nil {
		s.listener.Close()
		<-s.done
	}
	return s.closeError
}

func TestClusterCheckEnumeratesDirectedPairsAndReleasesAllSessions(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ports := checkPorts(t, "127.0.0.1")
	o := clusterInstallOptions{Hosts: []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, Binary: executable, HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Dir: "/tmp/new"}
	sessions := map[string]*fakeClusterCheckSession{}
	var output bytes.Buffer
	start := func(_ context.Context, node clusterInstallNode, _ []byte) (clusterCheckSession, error) {
		s := &fakeClusterCheckSession{host: node.Address, port: o.HTTP}
		sessions[node.Address] = s
		return s, nil
	}
	if err := clusterCheckWithStarter(t.Context(), o, &output, start); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]bool{}
	for _, host := range o.Hosts {
		s := sessions[host]
		prepare := s.requests[0]
		if prepare.Token != prepare.Targets[host] || tokens[prepare.Token] {
			t.Fatal("missing unique node challenge")
		}
		tokens[prepare.Token] = true
		if !s.closed {
			t.Fatal("session leaked")
		}
		if len(s.requests) != 7 {
			t.Fatalf("%s: want prepare + six directed probes, got %d", host, len(s.requests))
		}
		seen := map[string]bool{}
		for _, r := range s.requests[1:] {
			if r.Target == host {
				t.Fatal("included self path")
			}
			if r.Token != prepare.Targets[r.Target] || r.Token == prepare.Token {
				t.Fatal("probe authenticated source instead of target")
			}
			seen[fmt.Sprintf("%s:%d", r.Target, r.Port)] = true
		}
		if len(seen) != 6 {
			t.Fatalf("missing direction/port: %v", s.requests)
		}
	}
	if !strings.Contains(output.String(), "预检查通过：") {
		t.Fatal(output.String())
	}
}

func TestClusterCheckFailureKeepsDirectionAndCleanupErrors(t *testing.T) {
	executable, _ := os.Executable()
	ports := checkPorts(t, "127.0.0.1")
	o := clusterInstallOptions{Hosts: []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, Binary: executable, HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Dir: "/tmp/new"}
	sessions := map[string]*fakeClusterCheckSession{}
	cleanup := errors.New("scratch cleanup failed")
	start := func(_ context.Context, node clusterInstallNode, _ []byte) (clusterCheckSession, error) {
		s := &fakeClusterCheckSession{host: node.Address, port: o.HTTP}
		if node.Address == o.Hosts[0] {
			s.fail = o.Hosts[2]
			s.closeError = cleanup
		}
		sessions[node.Address] = s
		return s, nil
	}
	err := clusterCheckWithStarter(t.Context(), o, io.Discard, start)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1 -> 127.0.0.3 TCP:") || !errors.Is(err, cleanup) {
		t.Fatalf("lost directional/cleanup failure: %v", err)
	}
	for _, s := range sessions {
		if !s.closed {
			t.Fatal("session leaked on failure")
		}
	}
}

func TestClusterCheckCanceledDoesNotCreateRemoteResources(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := clusterCheckWithStarter(ctx, clusterInstallOptions{}, io.Discard, func(context.Context, clusterInstallNode, []byte) (clusterCheckSession, error) {
		t.Fatal("started after cancel")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel lost: %v", err)
	}
}

func TestClusterCheckOwnedScratchCleanupRejectsForeignResources(t *testing.T) {
	for _, tc := range []struct {
		name        string
		owner       string
		foreign     bool
		wantSuccess bool
	}{
		{"owned", "123456", false, true}, {"wrong-owner", "foreign", false, false}, {"foreign-file", "123456", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := filepath.Join(t.TempDir(), "gpuworks-cluster-check-123456")
			if err := os.Mkdir(scratch, 0700); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(scratch, "owner"), []byte(tc.owner), 0600)
			os.WriteFile(filepath.Join(scratch, "gpuworks"), []byte("copied-test-binary"), 0700)
			if tc.foreign {
				os.WriteFile(filepath.Join(scratch, "foreign"), []byte("preserve"), 0600)
			}
			err := exec.Command("sh", "-c", clusterCheckScratchCleanupCommand(scratch)).Run()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("wrong cleanup result: %v", err)
			}
			if tc.wantSuccess {
				if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("scratch leaked")
				}
			}
			if tc.owner == "foreign" {
				raw, err := os.ReadFile(filepath.Join(scratch, "gpuworks"))
				if err != nil || string(raw) != "copied-test-binary" {
					t.Fatal("deleted foreign scratch")
				}
			}
			if tc.foreign {
				raw, err := os.ReadFile(filepath.Join(scratch, "foreign"))
				if err != nil || string(raw) != "preserve" {
					t.Fatal("deleted foreign file")
				}
			}
		})
	}
}

func TestClusterCheckSSHSubprocess(t *testing.T) {
	if os.Getenv("GPUWORKS_CHECK_HELPER_SUBPROCESS") != "1" {
		return
	}
	err := clusterCheckNodeRun(context.Background(), os.Stdin, os.Stdout, func(context.Context, clusterCheckRequest) error { return nil })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestClusterCheckSSHSessionUsesOwnedScratchAndCleansOnCancel(t *testing.T) {
	fixture := t.TempDir()
	script := `#!/bin/sh
for command do :; done
case "$command" in
 *' cluster-check-node')
   export GPUWORKS_CHECK_HELPER_SUBPROCESS=1
   command="${command%cluster-check-node}-test.run=^TestClusterCheckSSHSubprocess$"
   ;;
esac
exec /bin/sh -c "$command"
`
	if err := os.WriteFile(filepath.Join(fixture, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fixture+string(os.PathListSeparator)+os.Getenv("PATH"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelEarly), func(t *testing.T) {
			ports := checkPorts(t, "127.0.0.1")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			session, err := startClusterCheckSSH(ctx, clusterInstallNode{Name: "fixture", Address: "127.0.0.1", SSH: clusterInstallSSH{Target: "fixture"}}, binary)
			if err != nil {
				t.Fatal(err)
			}
			ssh := session.(*clusterCheckSSHSession)
			token := strings.Repeat("a", 64)
			response, err := session.Exchange(clusterCheckRequest{Action: "prepare", Host: "127.0.0.1", HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Token: token, Targets: map[string]string{"127.0.0.1": token}})
			if err != nil || response.Error != "" {
				session.Close()
				t.Fatalf("prepare: %v %v", response, err)
			}
			if err := clusterCheckProbe(ctx, "127.0.0.1", ports[0], token); err != nil {
				session.Close()
				t.Fatal(err)
			}
			if cancelEarly {
				cancel()
			}
			err = session.Close()
			if !cancelEarly && err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(ssh.scratch); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch leaked %s: %v", ssh.scratch, err)
			}
			for _, port := range ports {
				l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if err != nil {
					t.Fatalf("remote probe survived session: %v", err)
				}
				l.Close()
			}
		})
	}
}

func TestClusterCheckPreparationChecksAllNodesAndSuppressesSuccess(t *testing.T) {
	executable, _ := os.Executable()
	ports := checkPorts(t, "127.0.0.1")
	o := clusterInstallOptions{Hosts: []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, Binary: executable, HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Dir: "/tmp/new"}
	sessions := map[string]*fakeClusterCheckSession{}
	var attempted []string
	var output bytes.Buffer
	start := func(_ context.Context, node clusterInstallNode, _ []byte) (clusterCheckSession, error) {
		attempted = append(attempted, node.Address)
		if node.Address == o.Hosts[1] {
			return nil, errors.New("SSH denied")
		}
		s := &fakeClusterCheckSession{host: node.Address, port: o.HTTP}
		sessions[node.Address] = s
		return s, nil
	}
	err := clusterCheckWithStarter(t.Context(), o, &output, start)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.2") || !strings.Contains(err.Error(), "SSH denied") {
		t.Fatalf("lost SSH node error: %v", err)
	}
	if len(attempted) != 3 {
		t.Fatalf("did not check later nodes: %v", attempted)
	}
	if strings.Contains(output.String(), "预检查通过：") {
		t.Fatal("reported success after preflight failure")
	}
	for _, s := range sessions {
		if !s.closed || len(s.requests) != 1 {
			t.Fatalf("leaked/probed incomplete preparation: %+v", s)
		}
	}
}

func TestClusterCheckCleanupFailureNeverReportsPassed(t *testing.T) {
	executable, _ := os.Executable()
	ports := checkPorts(t, "127.0.0.1")
	o := clusterInstallOptions{Hosts: []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, Binary: executable, HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Dir: "/tmp/new"}
	var output bytes.Buffer
	start := func(_ context.Context, node clusterInstallNode, _ []byte) (clusterCheckSession, error) {
		return &fakeClusterCheckSession{host: node.Address, port: o.HTTP, closeError: errors.New("retained scratch")}, nil
	}
	err := clusterCheckWithStarter(t.Context(), o, &output, start)
	if err == nil || strings.Contains(output.String(), "预检查通过：") {
		t.Fatalf("false success despite cleanup error: %v %s", err, output.String())
	}
}

func TestClusterCheckOccupiedMemberSessionPortReleasesOnlyOwnedListeners(t *testing.T) {
	ports := checkPorts(t, "127.0.0.1")
	blocker, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[3])))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	req := clusterCheckRequest{Action: "prepare", Host: "127.0.0.1", HTTP: ports[0], Enrollment: ports[1], Database: ports[2], Token: strings.Repeat("a", 64)}
	raw, _ := json.Marshal(req)
	var output bytes.Buffer
	if err := clusterCheckNodeRun(t.Context(), bytes.NewReader(raw), &output, func(context.Context, clusterCheckRequest) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var response clusterCheckResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Error, fmt.Sprintf("127.0.0.1:%d", ports[3])) {
		t.Fatalf("missing member-session bind failure: %s", response.Error)
	}
	for _, port := range ports[:3] {
		l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("partial bind leaked: %v", err)
		}
		l.Close()
	}
	if l, err := net.Listen("tcp4", blocker.Addr().String()); err == nil {
		l.Close()
		t.Fatal("stopped foreign member-session listener")
	}
}
