package pluginsdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// NodeIdentity reads the salted hardware identity of the named nodes (at
// most extapi.MaxNodeRead): for each that exists, in ascending id order,
// the machine it runs on and the cards it may use that carry a serial.
// The server binds salt to this package, so another package's digests of
// the same machine differ even with the same salt, and this package gets
// the same digests on every server. It needs the read:node_identity
// permission and the host.node_identity capability. A refusal by the server
// is an *Error.
func (p *Plugin) NodeIdentity(ctx context.Context, salt string, nodeIDs []uint64) (*extapi.NodeIdentityAnswer, error) {
	if err := p.canRead(extapi.CapHostNodeIdentity); err != nil {
		return nil, err
	}
	body, err := extapi.Marshal(extapi.NodeIdentityRequest{Salt: salt, NodeIDs: nodeIDs})
	if err != nil {
		return nil, err
	}
	raw, err := p.hostCall(ctx, http.MethodPost, extapi.PathHostNodeIdentity, body)
	if err != nil {
		return nil, err
	}
	return extapi.DecodeNodeIdentityAnswer(raw)
}

// Nodes reads one page of the node inventory in enrollment order: start
// with the zero query and pass each page's Next as the next query's Cursor
// until Next is empty. The server allows a few pages a second; read what a
// page of the console shows, not the whole fleet on every event. It needs
// the read:node_inventory permission and the host.nodes capability. A
// refusal by the server is an *Error.
func (p *Plugin) Nodes(ctx context.Context, q extapi.NodesQuery) (*extapi.NodesPage, error) {
	if err := p.canRead(extapi.CapHostNodes); err != nil {
		return nil, err
	}
	path := extapi.PathHostNodes
	if s := q.Encode(); s != "" {
		path += "?" + s
	}
	raw, err := p.hostCall(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return extapi.DecodeNodesPage(raw)
}

func (p *Plugin) canRead(capability string) error {
	if !p.ready.Load() {
		return errors.New("pluginsdk: read before initialize")
	}
	if _, ok := p.use[capability]; !ok {
		return fmt.Errorf("%w: %s", ErrNotNegotiated, capability)
	}
	return nil
}

// hostCall makes one host API call and returns its body, or the server's
// refusal as an *Error.
func (p *Plugin) hostCall(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+hostAuthority+path, r)
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReadAnswer+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxReadAnswer {
		return nil, &extapi.Error{Code: extapi.ErrorWireMalformed, Message: "answer too large",
			Params: map[string]any{"object": "answer", "field": "_root", "rule": extapi.RuleRange}}
	}
	return raw, nil
}

// maxReadAnswer bounds a read's answer: a page of 500 nodes with every
// card listed stays well within it.
const maxReadAnswer = 16 << 20
