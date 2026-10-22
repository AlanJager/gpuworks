package proto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// ServerDialChallenge is sent by a listening agent before yamux starts. The
// server must prove it knows the shared install token for this process.
type ServerDialChallenge struct {
	NodeUUID string `json:"node_uuid,omitempty"`
	Nonce    string `json:"nonce"`
}

// ServerDialProof is the server's response to ServerDialChallenge.
type ServerDialProof struct {
	HMAC string `json:"hmac"`
}

// ServerDialHMAC signs a server_dial challenge nonce with the shared secret.
func ServerDialHMAC(secret, nonce string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyServerDialHMAC checks a server_dial proof in constant time.
func VerifyServerDialHMAC(secret, nonce, proof string) bool {
	want := ServerDialHMAC(secret, nonce)
	return hmac.Equal([]byte(want), []byte(proof))
}

// ServerDialNodeSecret derives the post-registration reverse-auth secret from
// the node token. The server already stores this SHA-256 value for normal node
// authentication, so reconnects do not require persisting the plaintext token.
func ServerDialNodeSecret(nodeToken string) string {
	sum := sha256.Sum256([]byte(nodeToken))
	return hex.EncodeToString(sum[:])
}
