package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

type countingReader struct {
	r        io.Reader
	n        int64
	progress func(int64)
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	if n > 0 && r.progress != nil {
		r.progress(r.n)
	}
	return n, err
}

// CopyImageArchiveURL streams an image archive published over plain HTTP
// straight into a target agent's load stream through the management server
// when the node cannot reach the archive URL itself.
//
// Nothing is staged. The archive's body is copied from the HTTP response into
// the yamux image.write stream with the same 1 MiB buffer and the same
// counting reader the other relay paths use, so a fourteen-gigabyte archive
// costs the server one buffer, not one file. That is also why this transfer
// cannot resume: neither end holds a copy to continue from.
//
// The returned byte count is what made it across, on the failure path too, and
// the error text names it: an operator reading "0 bytes" knows the mirror is
// unreachable, while "9.1 GB" says the transfer broke partway.
func (h *Hub) CopyImageArchiveURL(ctx context.Context, archiveURL, targetNodeUUID, imageRef, transferID string, progress func(int64)) (int64, error) {
	normalized, err := imagecatalog.NormalizeArchiveURL(archiveURL)
	if err != nil {
		return 0, err
	}
	host := imagecatalog.ArchiveURLHost(normalized)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, normalized, nil)
	if err != nil {
		return 0, fmt.Errorf("request image archive from %s: %w", host, err)
	}
	client := imagecatalog.ArchiveStreamHTTPClient()
	client.Transport = outbound.Transport(func() (proto.NetworkConfig, error) { return planning.ControlPlaneNetwork(h.store) }, h.outboundAllowPrivateHosts...)
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("fetch image archive from %s: %w (0 bytes transferred)", host, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Deliberately before the node is asked for a load stream: an agent that
		// opened one would have to be told the archive never started.
		return 0, fmt.Errorf("image archive host %s answered %s (0 bytes transferred)", host, response.Status)
	}
	target := h.session(targetNodeUUID)
	if target == nil {
		return 0, errors.New("target node must be connected")
	}
	writeConn, err := target.openStream(ctx, proto.StreamHeader{Kind: proto.StreamImageWrite, Dst: imageRef, Ref: transferID})
	if err != nil {
		return 0, fmt.Errorf("open image load stream: %w", err)
	}
	defer writeConn.Close()
	reader := &countingReader{r: response.Body, progress: progress}
	if _, err := io.CopyBuffer(writeConn, reader, make([]byte, 1<<20)); err != nil {
		h.log.Warn("image archive relay failed", "target", targetNodeUUID, "image", imageRef, "transfer", transferID, "host", host, "bytes", reader.n, "err", err)
		return reader.n, fmt.Errorf("relay image archive from %s: %w (%d bytes transferred)", host, err, reader.n)
	}
	h.log.Info("image archive relay completed", "target", targetNodeUUID, "image", imageRef, "transfer", transferID, "host", host, "bytes", reader.n)
	// Closing the write side is the EOF signal consumed by the runtime's load.
	_ = writeConn.Close()
	select {
	case <-ctx.Done():
		return reader.n, ctx.Err()
	default:
	}
	return reader.n, nil
}
