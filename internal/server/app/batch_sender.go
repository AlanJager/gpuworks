package app

import (
	"context"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// batchSender sends a batch's requests through the gateway in process, as the
// batch owner's: routing, the wake hold and the accounting of a client's
// request, so a batch is traffic of the deployment it reaches.
type batchSender struct {
	proxy *proxy.Proxy
}

func (s batchSender) Send(ctx context.Context, owner batch.Owner, path string, body []byte) batch.Reply {
	r := s.proxy.Do(ctx, uxrecord.Caller{APIKeyID: owner.APIKeyID, UserID: owner.UserID, Source: uxrecord.SourceBatch}, path, body)
	return batch.Reply{Status: r.Status, RequestID: r.RequestID, Body: r.Body, RetryAfter: r.RetryAfter, TooLarge: r.TooLarge, Err: r.Err}
}

// batchAuth names the caller of a batch or file request from its API key, as
// the gateway does for a model request.
func batchAuth(px *proxy.Proxy) batch.Authenticate {
	return func(r *http.Request) (batch.Owner, bool) {
		caller, ok := px.Identify(r)
		return batch.Owner{UserID: caller.UserID, APIKeyID: caller.APIKeyID}, ok
	}
}
