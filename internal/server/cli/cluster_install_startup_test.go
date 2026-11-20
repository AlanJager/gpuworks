package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClusterInstallStartsJoinedPeersBeforeWaiting(t *testing.T) {
	for _, noService := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-service=%v", noService), func(t *testing.T) {
			o := offlineInstallOptions(t)
			o.NoService = noService
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			bothStarted := make(chan struct{})
			var mu sync.Mutex
			started, completed, joined, formal := 0, 0, 0, 0
			calls := make(map[string]int)
			remote := func(ctx context.Context, host string, input io.Reader) ([]byte, error) {
				var req clusterInstallNodeRequest
				if err := json.NewDecoder(input).Decode(&req); err != nil {
					return nil, err
				}
				mu.Lock()
				calls[req.Action+":"+host]++
				switch req.Action {
				case "join":
					joined++
				case "serve-joined", "service":
					startAction := "service"
					if noService {
						startAction = "serve-joined"
					}
					if host != o.Hosts[0] && req.Action == startAction {
						if joined != 2 {
							mu.Unlock()
							return nil, errors.New("control plane started before both sequential joins completed")
						}
						started++
						if started == 2 {
							close(bothStarted)
						}
						mu.Unlock()
						// A peer cannot finish readiness until the other peer has
						// started. Serial start-and-wait deterministically times out.
						select {
						case <-bothStarted:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						mu.Lock()
						completed++
					} else if !noService && req.Action == "serve-joined" {
						mu.Unlock()
						return nil, errors.New("joined node unnecessarily started a temporary server")
					} else if req.Action == "service" && completed != 2 {
						mu.Unlock()
						return nil, errors.New("seed service transitioned before joined peers were ready")
					}
				case "bootstrap", "status", "business", "read-setting":
					if completed != 2 {
						mu.Unlock()
						return nil, errors.New("installation verification ran before joined peers were ready")
					}
					formal++
				}
				mu.Unlock()
				return startupFixtureResponse(req.Action, true)
			}
			var output bytes.Buffer
			err := installCluster(ctx, o, &output, remote, func(context.Context, string, string, io.Reader) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if formal != 12 || !strings.Contains(output.String(), "passed status") {
				t.Fatalf("missing final validation: operations=%d, output=%s", formal, output.String())
			}
			for _, host := range o.Hosts[1:] {
				wantServe := 0
				if noService {
					wantServe = 1
				}
				if calls["serve-joined:"+host] != wantServe || calls["service:"+host] != 1 {
					t.Fatalf("duplicate or missing node transition: %v", calls)
				}
			}
		})
	}
}

func TestClusterInstallParallelStartupFailureCancelsAndJoinsPeers(t *testing.T) {
	for _, failure := range []string{"helper", "malformed", "cancel", "cancel-success"} {
		t.Run(failure, func(t *testing.T) {
			o := offlineInstallOptions(t)
			o.NoService = false
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			bothStarted := make(chan struct{})
			peerExited := make(chan struct{})
			var mu sync.Mutex
			started, later := 0, 0
			sentinel := errors.New("start rejected; owned logs retained")
			remote := func(callCtx context.Context, host string, input io.Reader) ([]byte, error) {
				var req clusterInstallNodeRequest
				if err := json.NewDecoder(input).Decode(&req); err != nil {
					return nil, err
				}
				if req.Action == "service" && host != o.Hosts[0] {
					if host == o.Hosts[2] {
						defer close(peerExited)
					}
					mu.Lock()
					started++
					if started == 2 {
						close(bothStarted)
					}
					mu.Unlock()
					select {
					case <-bothStarted:
					case <-callCtx.Done():
						return nil, callCtx.Err()
					}
					if failure == "cancel-success" {
						cancel()
						return startupFixtureResponse(req.Action, true)
					}
					if host == o.Hosts[1] {
						switch failure {
						case "helper":
							return nil, sentinel
						case "malformed":
							return []byte(`{"completed":`), nil
						case "cancel":
							cancel()
						}
					}
					<-callCtx.Done()
					return nil, callCtx.Err()
				}
				if req.Action == "bootstrap" || req.Action == "status" || req.Action == "business" || req.Action == "read-setting" || req.Action == "service" && host == o.Hosts[0] {
					mu.Lock()
					later++
					mu.Unlock()
				}
				return startupFixtureResponse(req.Action, true)
			}
			var output bytes.Buffer
			err := installCluster(ctx, o, &output, remote, func(context.Context, string, string, io.Reader) error { return nil })
			if err == nil || later != 0 || strings.Contains(output.String(), "passed status") {
				t.Fatalf("startup failure reached success: err=%v later=%d output=%s", err, later, output.String())
			}
			if failure == "helper" && !errors.Is(err, sentinel) {
				t.Fatalf("lost originating helper error: %v", err)
			}
			if strings.HasPrefix(failure, "cancel") && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost caller cancellation: %v", err)
			}
			select {
			case <-peerExited:
			default:
				t.Fatal("installer returned before the other SSH helper exited")
			}
		})
	}
}

func TestClusterInstallRequiresEveryHelperToAdvertiseDirectService(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			o := offlineInstallOptions(t)
			o.NoService = false
			var transitions []string
			remote := func(_ context.Context, host string, input io.Reader) ([]byte, error) {
				var req clusterInstallNodeRequest
				if err := json.NewDecoder(input).Decode(&req); err != nil {
					return nil, err
				}
				if req.Action == "serve-joined" || req.Action == "service" {
					transitions = append(transitions, req.Action+":"+host)
				}
				return startupFixtureResponse(req.Action, mixed && host != o.Hosts[2])
			}
			if err := installCluster(t.Context(), o, io.Discard, remote, func(context.Context, string, string, io.Reader) error { return nil }); err != nil {
				t.Fatal(err)
			}
			want := []string{"serve-joined:" + o.Hosts[1], "serve-joined:" + o.Hosts[2], "service:" + o.Hosts[0], "service:" + o.Hosts[1], "service:" + o.Hosts[2]}
			if fmt.Sprint(transitions) != fmt.Sprint(want) {
				t.Fatalf("legacy helper protocol changed: %v, want %v", transitions, want)
			}
		})
	}
}

func startupFixtureResponse(action string, direct bool) ([]byte, error) {
	switch action {
	case "capabilities":
		if direct {
			return []byte(`{"native_dqlite":true,"os":"linux","direct_joined_service":true}`), nil
		}
		return []byte(`{"native_dqlite":true,"os":"linux"}`), nil
	case "seed":
		return []byte(`{"cluster_id":"owned","ca_fingerprint":"expected-pin"}`), nil
	case "invite":
		return []byte(`{"token":"private-invitation"}`), nil
	case "bootstrap":
		return []byte(`{"bootstrap_ready":true}`), nil
	case "status":
		return []byte(`{"status":"Status report\nOverall: ready (business reads verified)\n"}`), nil
	case "prepare", "join", "serve-joined", "service", "business", "read-setting":
		return []byte(`{"completed":true}`), nil
	default:
		return nil, fmt.Errorf("unexpected helper action %q", action)
	}
}
