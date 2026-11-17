package pluginhost

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// outcomeKind is how one process run ended.
type outcomeKind int

const (
	// outcomeCrash: restart after a backoff.
	outcomeCrash outcomeKind = iota + 1
	// outcomeFailed: do not restart until started again (a refusal that
	// retrying cannot change, a binary of another package, no isolation).
	outcomeFailed
	// outcomeStopped: stopped on request.
	outcomeStopped
	// outcomeIdle: an on-demand plugin stopped for being idle.
	outcomeIdle
)

// outcome is a run's end, with the code and params the status shows.
type outcome struct {
	kind   outcomeKind
	code   string
	params map[string]any
	// connBroken marks a handshake whose connection broke.
	connBroken bool
}

// retryableReasons are refusal reasons a later attempt may not meet.
var retryableReasons = map[string]bool{
	extapi.ReasonBusy: true, extapi.ReasonUnavailable: true, extapi.ReasonDeadlineExceeded: true,
	extapi.ReasonInternal: true, extapi.ReasonCancelled: true,
}

// handshakeRequest builds initialize for spec with a fresh principal key.
func (h *Host) handshakeRequest(spec *Spec) (*extapi.HandshakeRequest, error) {
	key := make([]byte, extapi.PrincipalKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	var held []extapi.HeldState
	if h.cfg.State != nil {
		held = h.cfg.State.Held(spec.ID)
	}
	clock := extapi.Clock{Wall: extapi.NewTime(time.Now()), ExtClock: extapi.NewTime(time.Now())}
	if h.cfg.Clock != nil {
		clock = h.cfg.Clock()
	}
	req := &extapi.HandshakeRequest{
		Protocol:     extapi.Range{Min: extapi.ProtocolMajor, Max: extapi.ProtocolMajor},
		Core:         h.cfg.Core(),
		Plugin:       spec.Grant,
		Held:         held,
		Clock:        clock,
		PrincipalKey: key,
	}
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("pluginhost: the initialize request this server built is invalid: %w", err)
	}
	return req, nil
}

// handshake sends initialize and checks the answer. It returns the
// accepted answer, or the outcome that ends the run.
func (h *Host) handshake(ctx context.Context, spec *Spec, c *pluginConn, req *extapi.HandshakeRequest) (*extapi.HandshakeResponse, *outcome) {
	body, err := extapi.Marshal(req)
	if err != nil {
		return nil, &outcome{kind: outcomeFailed, code: messages.ErrorPluginStartFailed, params: map[string]any{"id": spec.ID, "detail": err.Error()}}
	}
	raw, err := c.lifecycle(ctx, http.MethodPost, extapi.PathInitialize, body, h.timing.InitTimeout)
	if err != nil {
		var refusal *Error
		if errors.As(err, &refusal) {
			kind := outcomeFailed
			if retryableReasons[refusal.Wire.Reason] {
				kind = outcomeCrash
			}
			return nil, &outcome{kind: kind, code: messages.ErrorPluginHandshakeRefused,
				params: map[string]any{"id": spec.ID, "plugin_code": refusal.Wire.Code, "reason": refusal.Wire.Reason}}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &outcome{kind: outcomeCrash, code: messages.ErrorPluginHandshakeTimeout,
				params: map[string]any{"id": spec.ID, "timeout_s": int(h.timing.InitTimeout / time.Second)}}
		}
		// The connection broke: usually the process is exiting, and its
		// exit says more than the broken call (runOnce reads it).
		return nil, &outcome{kind: outcomeCrash, code: messages.ErrorPluginStartFailed,
			params: map[string]any{"id": spec.ID, "detail": err.Error()}, connBroken: true}
	}
	resp, err := extapi.DecodeHandshakeResponse(raw)
	if err != nil {
		var x *extapi.Error
		params := map[string]any{"id": spec.ID}
		code := extapi.ErrorWireMalformed
		if errors.As(err, &x) {
			code = x.Code
			for k, v := range x.Params {
				params[k] = v
			}
		}
		return nil, &outcome{kind: outcomeFailed, code: code, params: params}
	}
	if o := checkAnswer(spec, req, resp); o != nil {
		return nil, o
	}
	return resp, nil
}

// checkAnswer refuses an answer from a binary that is not the installed
// package's, or that uses a capability version neither offered nor
// approved.
func checkAnswer(spec *Spec, req *extapi.HandshakeRequest, resp *extapi.HandshakeResponse) *outcome {
	for _, f := range []struct{ field, want, got string }{
		{"id", spec.ID, resp.ID},
		{"version", spec.Version, resp.Version},
		{"manifest_sha256", string(spec.ManifestSHA256), string(resp.ManifestSHA256)},
	} {
		if f.want != f.got {
			return &outcome{kind: outcomeFailed, code: messages.ErrorPluginHandshakeMismatch,
				params: map[string]any{"id": spec.ID, "field": f.field, "want": f.want, "got": f.got}}
		}
	}
	for capability, v := range resp.Use {
		offer, offered := req.Core.Offer[capability]
		approved, ok := spec.Approved[capability]
		if !offered || !offer.Contains(v) || !ok || v > approved {
			return &outcome{kind: outcomeFailed, code: messages.ErrorPluginCapabilityNotOffered,
				params: map[string]any{"id": spec.ID, "capability": capability, "version": v}}
		}
	}
	return nil
}
