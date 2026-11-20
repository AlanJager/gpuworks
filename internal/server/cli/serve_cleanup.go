package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// closeServerResources preserves a cleanup failure even when Run handled its
// cancellation cleanly.
func closeServerResources(runErr error, close func() error) error {
	if err := close(); err != nil {
		return errors.Join(runErr, fmt.Errorf("close server resources: %w", err))
	}
	return runErr
}

// clusterJoinWait reports the admitted member, keeps its database session
// until the command is stopped, and returns a failed session close.
func clusterJoinWait(ctx context.Context, output io.Writer, observed any, closeSession func() error) (resultErr error) {
	defer func() { resultErr = closeServerResources(resultErr, closeSession) }()
	if err := json.NewEncoder(output).Encode(observed); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}
