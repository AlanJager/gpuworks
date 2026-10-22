package provider

import (
	"context"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/store"
)

var ErrProviderInUse = errors.New("provider configuration is still in use")

// PreDeleteChecker optionally protects a provider's connection configuration.
// Checks are local and read-only; they do not delete or release resources.
type PreDeleteChecker interface {
	PreDeleteCheck(context.Context, uint) error
}

func (s *Service) PreDeleteCheck(ctx context.Context, providerID uint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := s.store.GetProvider(providerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // Preserve idempotent configuration deletion.
	}
	if err != nil {
		return err
	}
	driver, ok := s.driver(p.Kind)
	if !ok {
		return fmt.Errorf("driver %s not registered", p.Kind)
	}
	if checker, ok := driver.(PreDeleteChecker); ok {
		return checker.PreDeleteCheck(ctx, providerID)
	}
	return nil
}
