package store

import (
	"context"
	"errors"
)

// WithInitialCredentialsWrite admits initial credentials publication and holds
// ownership until actual file work and deferred cleanup return. Call check
// before each new file action; cleanup of opened resources must still finish.
func (s *Store) WithInitialCredentialsWrite(ctx context.Context, work func(check func() error) error) error {
	if work == nil {
		return errors.New("initial credentials work is required")
	}
	return s.withBusinessFileWrite(ctx, work)
}
