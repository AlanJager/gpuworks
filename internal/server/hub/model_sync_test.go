package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestCoordinateModelCopyUsesDirectAgentRPCs(t *testing.T) {
	type callRecord struct {
		node, method string
		params       any
		long         bool
	}
	var calls []callRecord
	call := func(_ context.Context, node, method string, params any, long bool) (json.RawMessage, error) {
		calls = append(calls, callRecord{node: node, method: method, params: params, long: long})
		switch method {
		case proto.RPCModelSyncListen:
			return mustJSON(t, proto.ModelSyncListenResult{
				Endpoints: []string{"10.0.0.2:39123"}, Fingerprint: strings.Repeat("a", 64), TransferID: "transfer-1",
			}), nil
		case proto.RPCModelSyncSend:
			return mustJSON(t, proto.ModelSyncTransferResult{Bytes: 1234, SHA256: strings.Repeat("b", 64)}), nil
		default:
			return nil, errors.New("unexpected method: " + method)
		}
	}
	result, err := coordinateModelCopy(context.Background(), proto.ModelSyncCopyRequest{
		TransferID: "transfer-1", Key: "7-9", SourceNodeUUID: "source", SourcePath: "src/model",
		TargetNodeUUID: "target", TargetPath: "dst/model", SourceIP: "10.0.0.1", TargetIP: "10.0.0.2", ExpectedFileBytes: 1000,
	}, strings.Repeat("c", 64), call)
	if err != nil {
		t.Fatal(err)
	}
	if result.TransferID != "transfer-1" || result.Bytes != 1234 || len(result.SHA256) != 64 {
		t.Fatalf("copy result = %+v", result)
	}
	if len(calls) != 2 || calls[0].method != proto.RPCModelSyncListen || calls[1].method != proto.RPCModelSyncSend {
		t.Fatalf("RPC calls = %+v", calls)
	}
	if calls[0].long || !calls[1].long {
		t.Fatalf("RPC timeout modes = listen:%v send:%v", calls[0].long, calls[1].long)
	}
	listen := calls[0].params.(proto.ModelSyncListenParams)
	send := calls[1].params.(proto.ModelSyncSendParams)
	if len(listen.Token) != 64 || listen.Token != send.Token {
		t.Fatalf("one-time token mismatch: listen=%q send=%q", listen.Token, send.Token)
	}
	// The target listens on, and the source dials from, the pair's network.
	if listen.BindIP != "10.0.0.2" || send.SourceIP != "10.0.0.1" || send.Endpoints[0] != "10.0.0.2:39123" {
		t.Fatalf("direct endpoint not preserved: listen=%+v send=%+v", listen, send)
	}
}

func TestCoordinateModelCopyStopsTargetAfterSenderFailure(t *testing.T) {
	var methods []string
	call := func(_ context.Context, _ string, method string, _ any, _ bool) (json.RawMessage, error) {
		methods = append(methods, method)
		switch method {
		case proto.RPCModelSyncListen:
			return mustJSON(t, proto.ModelSyncListenResult{
				Endpoints: []string{"10.0.0.2:39123"}, Fingerprint: strings.Repeat("a", 64), TransferID: "transfer-2",
			}), nil
		case proto.RPCModelSyncSend:
			return nil, errors.New("source disconnected")
		case proto.RPCModelSyncStop:
			return mustJSON(t, map[string]string{"status": "stopping"}), nil
		default:
			return nil, errors.New("unexpected method")
		}
	}
	_, err := coordinateModelCopy(context.Background(), proto.ModelSyncCopyRequest{
		TransferID: "transfer-2", Key: "7-9", SourceNodeUUID: "source", SourcePath: "src",
		TargetNodeUUID: "target", TargetPath: "dst",
	}, strings.Repeat("c", 64), call)
	if err == nil || !strings.Contains(err.Error(), "source disconnected") {
		t.Fatalf("copy error = %v", err)
	}
	want := []string{proto.RPCModelSyncListen, proto.RPCModelSyncSend, proto.RPCModelSyncStop}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("RPC order = %v, want %v", methods, want)
	}
}

func TestCoordinateModelCopyDoesNotDuplicateActiveTarget(t *testing.T) {
	var methods []string
	call := func(_ context.Context, _ string, method string, _ any, _ bool) (json.RawMessage, error) {
		methods = append(methods, method)
		return mustJSON(t, proto.ModelSyncListenResult{InProgress: true, TransferID: "existing-transfer"}), nil
	}
	result, err := coordinateModelCopy(context.Background(), proto.ModelSyncCopyRequest{
		TransferID: "new-transfer", Key: "7-9", SourceNodeUUID: "source", SourcePath: "src",
		TargetNodeUUID: "target", TargetPath: "dst",
	}, strings.Repeat("c", 64), call)
	if err != nil {
		t.Fatal(err)
	}
	if !result.InProgress || result.TransferID != "existing-transfer" {
		t.Fatalf("copy result = %+v", result)
	}
	if len(methods) != 1 || methods[0] != proto.RPCModelSyncListen {
		t.Fatalf("active target triggered extra calls: %v", methods)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A listen that does not yield a usable receiver must still release the
// target. The agent registers the receiver and reports the model file as
// syncing before it answers, so a lost reply, an expired caller context or a
// malformed answer leaves a receiver that makes every retry answer InProgress
// until its 10-minute TTL expires — the model file sits in syncing with
// nothing downloaded (todo T23). Only an InProgress answer is left alone: that
// receiver belongs to a transfer an earlier request already owns.
func TestCoordinateModelCopyReleasesTheTargetWhenListenYieldsNoReceiver(t *testing.T) {
	for _, tc := range []struct {
		name      string
		listen    func() (json.RawMessage, error)
		wantError string
	}{
		{
			name:      "listen call fails after the receiver started",
			listen:    func() (json.RawMessage, error) { return nil, errors.New("context deadline exceeded") },
			wantError: "start target model receiver",
		},
		{
			name:      "listen answer does not decode",
			listen:    func() (json.RawMessage, error) { return json.RawMessage("not json"), nil },
			wantError: "decode target model receiver",
		},
		{
			name: "listen answer carries no endpoint",
			listen: func() (json.RawMessage, error) {
				return mustJSON(t, proto.ModelSyncListenResult{TransferID: "transfer-4"}), nil
			},
			wantError: "no endpoint or fingerprint",
		},
		{
			name: "listen answer carries no fingerprint",
			listen: func() (json.RawMessage, error) {
				return mustJSON(t, proto.ModelSyncListenResult{Endpoints: []string{"10.0.0.2:39123"}, TransferID: "transfer-4"}), nil
			},
			wantError: "no endpoint or fingerprint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var methods []string
			var stopped proto.ModelSyncStopParams
			var stopNode string
			call := func(_ context.Context, node, method string, params any, _ bool) (json.RawMessage, error) {
				methods = append(methods, method)
				switch method {
				case proto.RPCModelSyncListen:
					return tc.listen()
				case proto.RPCModelSyncStop:
					stopNode = node
					stopped = params.(proto.ModelSyncStopParams)
					return mustJSON(t, map[string]string{"status": "stopping"}), nil
				default:
					return nil, errors.New("unexpected method: " + method)
				}
			}
			result, err := coordinateModelCopy(context.Background(), proto.ModelSyncCopyRequest{
				TransferID: "transfer-4", Key: "7-9", SourceNodeUUID: "source", SourcePath: "src",
				TargetNodeUUID: "target", TargetPath: "dst",
			}, strings.Repeat("c", 64), call)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("copy error = %v, want one mentioning %q", err, tc.wantError)
			}
			if result.InProgress {
				t.Fatalf("a failed listen reported an in-progress transfer: %+v", result)
			}
			want := []string{proto.RPCModelSyncListen, proto.RPCModelSyncStop}
			if strings.Join(methods, ",") != strings.Join(want, ",") {
				t.Fatalf("RPC order = %v, want %v", methods, want)
			}
			// The stop has to name the same receiver the listen would have
			// created, or the agent refuses it as a transfer-id mismatch.
			if stopNode != "target" || stopped.Key != "7-9" || stopped.TransferID != "transfer-4" {
				t.Fatalf("stop sent to %q with %+v, want the target's own key and transfer id", stopNode, stopped)
			}
		})
	}
}
