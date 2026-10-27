package benchrun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/tokenizer"
)

// Generated token-length workloads cannot silently become character-based
// workloads. Natural imported/corpus samples can still run without a tokenizer;
// their local counts remain estimates, separate from endpoint-reported usage.
func (e *execution) loadTokenizer(ctx context.Context) error {
	e.dc.Mode = "estimate"
	ref := strings.TrimSpace(e.run.TokenizerRef)
	if ref != "" {
		loaded, err := tokenizer.LoadRef(ctx, ref)
		if err != nil {
			e.hooks.Warn(proto.BenchmarkWarningTokenizerLoad)
			return tokenizerLoadError(err)
		}
		e.tok, e.dc.Mode = loaded, "exact"
	}
	if (e.dc.Kind == "random" || e.dc.Kind == "agentic") && e.tok == nil {
		return fmt.Errorf("%s token-length workload requires a tokenizer; select a model repository or upload tokenizer.json before evaluation", e.dc.Kind)
	}
	return nil
}

// Load failures cross the persisted status boundary; URLs are not diagnostics.
func tokenizerLoadError(err error) error {
	cause := err
	var transport *url.Error
	if errors.As(err, &transport) {
		cause = transport.Err
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("tokenizer loading canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("tokenizer loading timed out: %w", context.DeadlineExceeded)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return fmt.Errorf("DNS resolution failed while downloading tokenizer; evaluation was not started: %s", loadgen.RedactErrorText(dns.Error()))
	}
	var network net.Error
	if errors.As(cause, &network) {
		return fmt.Errorf("network connection failed while downloading tokenizer; evaluation was not started: %s", loadgen.RedactErrorText(network.Error()))
	}
	if errors.Is(err, tokenizer.ErrDownload) {
		return fmt.Errorf("tokenizer download failed; evaluation was not started: %s", loadgen.RedactErrorText(cause.Error()))
	}
	return fmt.Errorf("tokenizer could not be loaded; evaluation was not started: %s", loadgen.RedactErrorText(cause.Error()))
}
