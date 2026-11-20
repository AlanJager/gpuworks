package cli

import (
	"errors"
	"fmt"
)

// closeServerResources preserves a cleanup failure even when Run handled its
// cancellation cleanly.
func closeServerResources(runErr error, close func() error) error {
	if err := close(); err != nil {
		return errors.Join(runErr, fmt.Errorf("close server resources: %w", err))
	}
	return runErr
}
