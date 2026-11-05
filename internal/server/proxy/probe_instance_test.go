package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInstanceProbePinsOneReplicaWithoutAccountingOrFallback(t *testing.T) {
	var requests atomic.Int64
	p, acct, _, id, closeUpstream := newAccountingProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer closeUpstream()
	first, err := p.store.InstanceByUUID("omni-instance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.CreateInstance(&store.Instance{UUID: "other-instance", DeploymentID: id, NodeID: first.NodeID, State: proto.InstRunning, APIPort: 30001}); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		reply := p.ProbeForInstance(context.Background(), id, "omni-instance", chatPath, map[string]any{"max_tokens": 1})
		if reply.HTTPStatus != 200 || reply.InstanceUUID != "omni-instance" || reply.Message != "" {
			t.Fatalf("probe was rerouted: %+v", reply)
		}
	}
	if requests.Load() != 4 {
		t.Fatalf("requests = %d", requests.Load())
	}
	if _, samples := acct.snapshotAndClearDirty(); len(samples) != 0 {
		t.Fatalf("probes were accounted: %+v", samples)
	}
	if reply := p.ProbeForInstance(context.Background(), id+1, "omni-instance", chatPath, nil); !reply.NoReplica {
		t.Fatalf("probe crossed deployment ownership: %+v", reply)
	}
	if reply := p.ProbeForInstance(context.Background(), id, "missing", chatPath, nil); !reply.NoReplica {
		t.Fatalf("missing instance fell back to another: %+v", reply)
	}
	if requests.Load() != 4 {
		t.Fatal("a refused pinned probe reached another replica")
	}
}

func TestInstanceProbeRejectsInterruptedHTTP200Bodies(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "short body", true: "cancellation after headers"}[canceled], func(t *testing.T) {
			release := make(chan struct{})
			p, _, _, id, closeUpstream := newAccountingProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if canceled {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "short")
			}))
			defer closeUpstream()
			defer close(release)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if deadline, ok := t.Deadline(); ok {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, deadline)
				defer stop()
			}
			if canceled {
				base := p.rp.Transport
				p.rp.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
					response, err := base.RoundTrip(request)
					if err == nil {
						response.Body = &cancelProbeBody{ReadCloser: response.Body, cancel: cancel}
					}
					return response, err
				})
			}
			reply := p.ProbeForInstance(ctx, id, "omni-instance", chatPath, map[string]any{"max_tokens": 1})
			if reply.HTTPStatus != 200 || reply.Message == "" || reply.InstanceUUID != "omni-instance" {
				t.Fatalf("an interrupted HTTP200 response appeared complete: %+v", reply)
			}
		})
	}
}

// ReverseProxy writes the response status before its first body read. Cancel
// at that exact boundary, so slow CI setup cannot turn this into a dial timeout.
type cancelProbeBody struct {
	io.ReadCloser
	once   sync.Once
	cancel context.CancelFunc
}

func (body *cancelProbeBody) Read(buffer []byte) (int, error) {
	body.once.Do(body.cancel)
	return body.ReadCloser.Read(buffer)
}
