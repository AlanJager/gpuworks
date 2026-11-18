package pluginsdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// ErrNotNegotiated is returned for a push of a kind whose table capability
// the handshake did not settle on.
var ErrNotNegotiated = errors.New("pluginsdk: the table capability was not negotiated")

// PushResult is the outcome of one PushState.
type PushResult struct {
	// Ack is the server's acknowledgement; nil when the push was skipped.
	Ack *extapi.StateAck
	// Skipped is true when the server already held this exact table, so
	// nothing was sent.
	Skipped bool
	// Generation and Digest are the push's (or, when skipped, the held
	// table's).
	Generation uint64
	Digest     extapi.Digest
}

// PushState replaces the plugin's whole table of kind with body (the kind's
// row type at the negotiated schema, such as *extapi.BannersV1). The push
// is skipped when the server already holds a table with the same digest.
// A refusal by the server is an *Error.
func (p *Plugin) PushState(ctx context.Context, kind string, body any) (PushResult, error) {
	if !p.ready.Load() {
		return PushResult{}, errors.New("pluginsdk: push before initialize")
	}
	schema, ok := p.use[extapi.TableCapability(kind)]
	if !ok {
		return PushResult{}, fmt.Errorf("%w: %s", ErrNotNegotiated, extapi.TableCapability(kind))
	}
	canonical, err := extapi.Marshal(body)
	if err != nil {
		return PushResult{}, fmt.Errorf("pluginsdk: encoding the %s table: %w", kind, err)
	}
	if len(canonical) == 0 || canonical[0] != '{' {
		return PushResult{}, fmt.Errorf("pluginsdk: the %s table is not a JSON object", kind)
	}
	digest := extapi.DigestOf(canonical)

	p.pushMu.Lock()
	defer p.pushMu.Unlock()
	held, has := p.held[kind]
	if has && held.Schema == schema && held.Digest == digest {
		return PushResult{Skipped: true, Generation: held.Generation, Digest: digest}, nil
	}
	gen := max(p.own[kind], held.Generation) + 1
	p.own[kind] = gen
	envelope, err := extapi.Marshal(extapi.StatePush{Kind: kind, Schema: schema, Generation: gen, Body: canonical})
	if err != nil {
		return PushResult{}, err
	}
	if len(envelope) > extapi.MaxPushBytes {
		return PushResult{}, &extapi.Error{Code: extapi.ErrorStateTooLarge, Message: "the push exceeds the size limit",
			Params: map[string]any{"kind": kind, "field": "body", "max": extapi.MaxPushBytes}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://"+hostAuthority+extapi.PathHostState+kind, bytes.NewReader(envelope))
	if err != nil {
		return PushResult{}, err
	}
	req.Header.Set("Content-Type", extapi.ContentTypeJSON)
	setDeadline(ctx, req.Header)
	resp, err := p.client.Do(req)
	if err != nil {
		return PushResult{}, fmt.Errorf("pluginsdk: pushing %s: %w", kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return PushResult{}, readError(resp)
	}
	raw, err := readBody(resp.Body)
	if err != nil {
		return PushResult{}, err
	}
	ack, err := extapi.DecodeStateAck(raw)
	if err != nil {
		return PushResult{}, fmt.Errorf("pluginsdk: the server's acknowledgement of %s: %w", kind, err)
	}
	if ack.Kind != kind || ack.Generation != gen {
		return PushResult{}, fmt.Errorf("pluginsdk: the server acknowledged %s generation %d for a push of %s generation %d",
			ack.Kind, ack.Generation, kind, gen)
	}
	if ack.AppliedAt != nil {
		p.held[kind] = extapi.HeldState{Kind: kind, Schema: schema, Generation: gen, Digest: ack.Digest}
	}
	return PushResult{Ack: ack, Generation: gen, Digest: digest}, nil
}

// HostHealth probes the host API.
func (p *Plugin) HostHealth(ctx context.Context) (*extapi.Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+hostAuthority+extapi.PathHostHealth, nil)
	if err != nil {
		return nil, err
	}
	setDeadline(ctx, req.Header)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, readError(resp)
	}
	raw, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	return extapi.DecodeHealth(raw)
}
