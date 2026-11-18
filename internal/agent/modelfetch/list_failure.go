package modelfetch

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// ListingErrorCode classifies the final listing failure, never an in-flight
// retry. Unknown or mixed source failures retain their complete detail without
// inventing one cause for the whole task. It does not inspect error text.
func ListingErrorCode(err error) string {
	if err == nil || errors.Is(err, context.Canceled) {
		return ""
	}
	if group, ok := err.(interface{ Unwrap() []error }); ok {
		code := ""
		for i, cause := range group.Unwrap() {
			next := ListingErrorCode(cause)
			if next == "" || (i > 0 && next != code) {
				return ""
			}
			code = next
		}
		return code
	}
	if status, ok := err.(*modelrepo.StatusError); ok {
		switch status.Code {
		case 404, 410:
			return proto.ErrorModelDownloadSourceNotFound
		case 401, 403:
			return proto.ErrorModelDownloadSourceAccessDenied
		case 429:
			return proto.ErrorModelDownloadSourceThrottled
		case 408, 500, 502, 503, 504:
			return proto.ErrorModelDownloadNetworkExhausted
		case 400, 405, 422, 501, 505:
			return proto.ErrorModelDownloadSourceRejected
		default:
			return ""
		}
	}
	if transport, ok := err.(net.Error); ok && (transport.Timeout() || transport.Temporary()) { //nolint:staticcheck // Legacy transports still report temporary DNS/connection failures this way.
		return proto.ErrorModelDownloadNetworkExhausted
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
		return ListingErrorCode(wrapped.Unwrap())
	}
	for _, cause := range []error{context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.EPIPE} {
		if errors.Is(err, cause) {
			return proto.ErrorModelDownloadNetworkExhausted
		}
	}
	return ""
}
