package modelmeta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrTokenRejected is a hub answering that a token is not valid.
var ErrTokenRejected = errors.New("the hub rejected the token")

// CheckHubToken asks a Hugging Face style hub at host who a token belongs to
// (GET https://<host>/api/whoami-v2), which is how a stored token is tested
// before a node needs it. A 401 or 403 is ErrTokenRejected; anything else
// that is not a 200 is a failure to reach the hub.
func CheckHubToken(ctx context.Context, client *http.Client, host, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/api/whoami-v2", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", host, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s answered %d", ErrTokenRejected, host, resp.StatusCode)
	default:
		return fmt.Errorf("%s answered %d to a token check", host, resp.StatusCode)
	}
}
