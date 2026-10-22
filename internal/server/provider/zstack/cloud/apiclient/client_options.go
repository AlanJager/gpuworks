package apiclient

import (
	"fmt"
	"log/slog"
	"time"
)

// ClientOption configures a Cloud client or overrides one Get/PutAction call.
type ClientOption func(*clientPolicy)

// ValidateOptions checks configuration before credentials are available.
func ValidateOptions(options ...ClientOption) error {
	_, err := defaultClientPolicy().override(options)
	return err
}

type clientPolicy struct {
	requestTimeout   time.Duration
	operationTimeout time.Duration
	pollInterval     time.Duration
	readRetries      int
	retryInitial     time.Duration
	retryMax         time.Duration
	serverTimeout    time.Duration
	logger           *slog.Logger
}

func defaultClientPolicy() clientPolicy {
	return clientPolicy{
		// Preserve the existing time budgets and opt in to query retries.
		requestTimeout: 30 * time.Second, operationTimeout: 30 * time.Second,
		pollInterval: time.Second, retryInitial: 100 * time.Millisecond,
		retryMax: 2 * time.Second,
	}
}

func WithRequestTimeout(timeout time.Duration) ClientOption {
	return func(p *clientPolicy) { p.requestTimeout = timeout }
}

// WithOperationTimeout bounds submission, retries and polling together.
func WithOperationTimeout(timeout time.Duration) ClientOption {
	return func(p *clientPolicy) { p.operationTimeout = timeout }
}

func WithPollInterval(interval time.Duration) ClientOption {
	return func(p *clientPolicy) { p.pollInterval = interval }
}

// WithReadRetries counts additional attempts after a failed GET. Zero disables
// retries. Mutations are never automatically resubmitted.
func WithReadRetries(retries int) ClientOption {
	return func(p *clientPolicy) { p.readRetries = retries }
}

func WithRetryBackoff(initial, maximum time.Duration) ClientOption {
	return func(p *clientPolicy) { p.retryInitial, p.retryMax = initial, maximum }
}

// WithServerAPITimeout sets ZStack's X-API-Timeout for mutations. Zero leaves
// Cloud's execution timeout unchanged; it is independent of local waiting.
func WithServerAPITimeout(timeout time.Duration) ClientOption {
	return func(p *clientPolicy) { p.serverTimeout = timeout }
}

// WithClientLogger enables structured debug records without request bodies,
// query parameters, credentials or raw remote error messages.
func WithClientLogger(logger *slog.Logger) ClientOption {
	return func(p *clientPolicy) { p.logger = logger }
}

func (p clientPolicy) override(options []ClientOption) (clientPolicy, error) {
	for _, option := range options {
		if option == nil {
			return p, fmt.Errorf("zstack: nil client option")
		}
		option(&p)
	}
	if p.requestTimeout <= 0 || p.operationTimeout <= 0 || p.pollInterval <= 0 {
		return p, fmt.Errorf("zstack: request timeout, operation timeout and poll interval must be positive")
	}
	if p.readRetries < 0 || p.retryInitial <= 0 || p.retryMax < p.retryInitial {
		return p, fmt.Errorf("zstack: invalid query retry count or backoff bounds")
	}
	if p.serverTimeout < 0 {
		return p, fmt.Errorf("zstack: server API timeout must not be negative")
	}
	return p, nil
}
