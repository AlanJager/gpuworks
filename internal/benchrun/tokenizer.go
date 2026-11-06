package benchrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
		loaded, err := e.loadTokenizerRef(ctx, ref)
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

// loadTokenizerRef loads ref. An engine reference asks the target itself
// (tokenizer.LoadEngine); when the target does not serve tokenization and the
// reference names a fallback, the fallback is loaded instead.
func (e *execution) loadTokenizerRef(ctx context.Context, ref string) (*tokenizer.Tokenizer, error) {
	fallback, engine := tokenizer.IsEngineRef(ref)
	if !engine {
		return tokenizer.LoadRef(ctx, ref)
	}
	base, _ := loadgen.NormalizeBaseURL(e.run.Client.BaseURL)
	tok, err := tokenizer.LoadEngine(ctx, tokenizer.Engine{HTTP: e.run.Client.HTTP, BaseURL: base, APIKey: e.run.Client.APIKey, Model: e.run.Model}, tokenizerSeed(e.run.ID))
	if err == nil || fallback == "" || ctx.Err() != nil {
		return tok, err
	}
	return tokenizer.LoadRef(ctx, fallback)
}

// EngineTokenizerRef is the reference naming the target's own tokenizer
// (tokenizer.EngineRef); what follows it is the fallback reference.
const EngineTokenizerRef = tokenizer.EngineRef

// TiktokenFile is the tiktoken vocabulary's file name in a model directory.
const TiktokenFile = tokenizer.TiktokenFile

// IsTiktokenVocabulary reports whether raw is a tiktoken vocabulary a run
// can load.
func IsTiktokenVocabulary(raw []byte) bool {
	_, err := tokenizer.LoadTiktoken(bytes.NewReader(raw))
	return err == nil
}

// tokenizerSeed fixes which vocabulary entries an engine sample draws for a
// run, so a rerun of the same run samples the same prompts.
func tokenizerSeed(runID uint) int64 { return int64(runID) + 1 }

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
	// A missing file is not a network failure, although the errno that says
	// so satisfies net.Error.
	if errors.Is(err, fs.ErrNotExist) {
		var path *fs.PathError
		if errors.As(err, &path) {
			return fmt.Errorf("tokenizer file not found: %s; evaluation was not started", path.Path)
		}
		return fmt.Errorf("tokenizer file not found; evaluation was not started: %s", loadgen.RedactErrorText(cause.Error()))
	}
	if errors.Is(err, tokenizer.ErrEngineTokenize) {
		return fmt.Errorf("the target does not serve /v1/tokenize and /v1/detokenize, so its tokenizer cannot be used; choose another tokenizer source; evaluation was not started: %s", loadgen.RedactErrorText(cause.Error()))
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
