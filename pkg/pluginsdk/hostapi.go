package pluginsdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Meta reads the server's version, boot id and clocks (host.clock).
func (p *Plugin) Meta(ctx context.Context) (*extapi.HostMeta, error) {
	raw, err := p.hostAPI(ctx, extapi.CapHostClock, http.MethodGet, extapi.PathHostMeta, nil)
	if err != nil {
		return nil, err
	}
	return extapi.DecodeHostMeta(raw)
}

// CorrectClock moves the server's extclock to to (host.clock, and the
// clock_authority role the package's certificate or the host operator
// grants). Call it only with a time the plugin has verified; reason goes to
// the server's audit log. A refusal is an *Error, for example
// PLUGIN_CLOCK_CORRECTION_TOO_LARGE for a move of more than 366 days.
func (p *Plugin) CorrectClock(ctx context.Context, to time.Time, reason string) (*extapi.ClockCorrected, error) {
	req := extapi.ClockCorrection{To: extapi.NewTime(to), Reason: reason}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body, err := extapi.Marshal(req)
	if err != nil {
		return nil, err
	}
	raw, err := p.hostAPI(ctx, extapi.CapHostClock, http.MethodPost, extapi.PathHostClockCorrect, body)
	if err != nil {
		return nil, err
	}
	return extapi.DecodeClockCorrected(raw)
}

// Ready tells the server this process has finished starting: if it goes
// down later, its grants clamp as soon as their grace ends, without the
// boot extension. Call it once the plugin has pushed what it must push
// after a start. A second call changes nothing.
func (p *Plugin) Ready(ctx context.Context) error {
	_, err := p.hostAPI(ctx, "", http.MethodPost, extapi.PathHostReady, nil)
	return err
}

// PushedState reads back the plugin's own table of kind as the server holds
// it (host.state_read); Held reports whether there is one. It is how a
// restarted plugin whose storage was lost or restored reconciles.
func (p *Plugin) PushedState(ctx context.Context, kind string) (*extapi.PushedState, error) {
	raw, err := p.hostAPI(ctx, extapi.CapHostStateRead, http.MethodGet, extapi.PathHostState+kind, nil)
	if err != nil {
		return nil, err
	}
	held, err := extapi.DecodePushedState(raw)
	if err != nil {
		return nil, err
	}
	if held.Kind != kind {
		return nil, fmt.Errorf("pluginsdk: the server answered a read of %s with %s", kind, held.Kind)
	}
	return held, nil
}

// DeriveKey returns a key of length bytes bound to this package and label
// (crypto:derive_key, host.derive_key). The same label gives the same key
// for the life of the server's master key; the plugin never sees the master
// key.
func (p *Plugin) DeriveKey(ctx context.Context, label string, length int) ([]byte, error) {
	req := extapi.KeyRequest{Label: label, Length: length}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body, err := extapi.Marshal(req)
	if err != nil {
		return nil, err
	}
	raw, err := p.hostAPI(ctx, extapi.CapHostDeriveKey, http.MethodPost, extapi.PathHostDeriveKey, body)
	if err != nil {
		return nil, err
	}
	k, err := extapi.DecodeDerivedKey(raw)
	if err != nil {
		return nil, err
	}
	if len(k.Key) != length {
		return nil, fmt.Errorf("pluginsdk: the server derived %d bytes for a key of %d", len(k.Key), length)
	}
	return k.Key, nil
}

// hostAPI makes one host API call after checking the handshake was accepted
// and capability (unless empty) negotiated; a refusal is an *Error.
func (p *Plugin) hostAPI(ctx context.Context, capability, method, path string, body []byte) ([]byte, error) {
	if !p.ready.Load() {
		return nil, errors.New("pluginsdk: host API call before initialize")
	}
	if capability != "" {
		if _, ok := p.use[capability]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotNegotiated, capability)
		}
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+hostAuthority+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", extapi.ContentTypeJSON)
	}
	setDeadline(ctx, req.Header)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pluginsdk: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, readError(resp)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	// An own-state answer carries a whole pushed table.
	limit := int64(maxCallBody)
	if method == http.MethodGet && path != extapi.PathHostMeta {
		limit = extapi.MaxPushedStateBytes
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, &extapi.Error{Code: extapi.ErrorWireMalformed, Message: "answer too large",
			Params: map[string]any{"object": "answer", "field": "_root", "rule": extapi.RuleRange}}
	}
	return raw, nil
}
