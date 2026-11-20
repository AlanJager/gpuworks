package enrollment

import (
	"context"
	"encoding/json"
	"errors"
)

// RegisterClusterEnrollmentOrigin publishes this member's own HTTPS endpoint
// through a pinned peer. The caller must retain its durable member identity
// and must supply the intended peer's current admitted key.
func (c *Client) RegisterClusterEnrollmentOrigin(ctx context.Context, certificate, key []byte, origin, expectedPeerKey string) error {
	if !canonicalEnrollmentOrigin(origin) || expectedPeerKey == "" {
		return errors.New("registration requires a canonical origin and expected member key")
	}
	body, err := json.Marshal(struct {
		Origin string `json:"origin"`
	}{Origin: origin})
	if err != nil {
		return err
	}
	response, err := c.memberRequestForPeerWithBody(ctx, certificate, key, ClusterEnrollmentOriginPath, body, 1, expectedPeerKey, 204)
	if err != nil {
		return err
	}
	if len(response) != 0 {
		return errors.New("enrollment origin registration returned a body")
	}
	return nil
}
