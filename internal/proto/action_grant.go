package proto

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ActionGrantVersion      = 2
	ActionGrantMaxTTL       = 10 * time.Second
	ActionGrantMaxClockSkew = 2 * time.Second
)

const actionGrantDomain = "gpuworks.action-grant.v2\x00"

// ActionGrantScope identifies one action against one agent connection. The
// caller must hash the canonical payload it will actually execute and compare
// this entire scope at the execution boundary. ResourceVersion may be zero for
// actions without a persisted resource version; PayloadSHA256 is always needed.
// Instance run/stop also carry TargetSHA256, which omits only the transient
// runtime asset Content bytes from the exact wire body.
type ActionGrantScope struct {
	ClusterID            string `json:"cluster_id"`
	NodeUUID             string `json:"node_uuid"`
	IssuerMemberID       uint64 `json:"issuer_member_id"`
	SchedulingGeneration uint64 `json:"scheduling_generation"`
	OwnerMemberID        uint64 `json:"owner_member_id"`
	SessionGeneration    uint64 `json:"session_generation"`
	SessionUUID          string `json:"session_uuid"`
	ConnectionID         string `json:"connection_id"`
	ActionFamily         string `json:"action_family"`
	ResourceID           string `json:"resource_id"`
	ResourceVersion      uint64 `json:"resource_version"`
	PayloadSHA256        string `json:"payload_sha256"`
	TargetSHA256         string `json:"target_sha256,omitempty"`
}

// ActionGrantClaims are signed with a versioned, length-prefixed binary form.
// KeyID names a separately trusted public key, not a secret derived from the
// cluster ID. GrantID uniqueness and replay accounting belong to the issuer
// and agent; this type does not provide either by itself.
type ActionGrantClaims struct {
	Version uint8 `json:"version"`
	ActionGrantScope
	KeyID          string `json:"key_id"`
	GrantID        string `json:"grant_id"`
	IssuedAtUnixMS int64  `json:"issued_at_unix_ms"`
	NotAfterUnixMS int64  `json:"not_after_unix_ms"`
}

type ActionGrant struct {
	Claims    ActionGrantClaims `json:"claims"`
	Signature []byte            `json:"signature"`
}

// ActionGrantPresentation travels beside, rather than inside, a desired
// target so the target's payload digest excludes the authorization bytes.
type ActionGrantPresentation struct {
	Grant ActionGrant            `json:"grant"`
	Proof ActionGrantQuorumProof `json:"proof"`
}

// SignActionGrant signs only a structurally valid, short-lived ticket. It does
// not establish quorum, key authority, or the issuing member's live role.
func SignActionGrant(claims ActionGrantClaims, privateKey ed25519.PrivateKey) (ActionGrant, error) {
	if err := ValidateActionGrantClaims(claims); err != nil {
		return ActionGrant{}, err
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return ActionGrant{}, errors.New("invalid action grant private key")
	}
	return ActionGrant{Claims: claims, Signature: ed25519.Sign(privateKey, actionGrantSigningBytes(claims))}, nil
}

// ValidateActionGrantClaims checks a proposed ticket before a replicated
// issuer reservation. It does not establish current leadership or authority.
func ValidateActionGrantClaims(claims ActionGrantClaims) error {
	return validateActionGrantClaims(claims)
}

// VerifyActionGrant checks the signature, trusted key selection, exact action
// binding, and agent-clock validity. Expiration has no grace period: the 2 s
// clock bound is for future issuance only. A new leader must still wait the
// maximum TTL plus the clock bound and drain already started actions.
func VerifyActionGrant(grant ActionGrant, publicKey ed25519.PublicKey, expectedKeyID string, expected ActionGrantScope, now time.Time) error {
	if err := validateActionGrantClaims(grant.Claims); err != nil {
		return err
	}
	if expectedKeyID == "" || grant.Claims.KeyID != expectedKeyID || grant.Claims.ActionGrantScope != expected {
		return errors.New("action grant binding mismatch")
	}
	if len(publicKey) != ed25519.PublicKeySize || len(grant.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, actionGrantSigningBytes(grant.Claims), grant.Signature) {
		return errors.New("invalid action grant signature")
	}
	issued := time.UnixMilli(grant.Claims.IssuedAtUnixMS)
	if now.Before(issued.Add(-ActionGrantMaxClockSkew)) {
		return errors.New("action grant issued too far in the future")
	}
	if !now.Before(time.UnixMilli(grant.Claims.NotAfterUnixMS)) {
		return errors.New("action grant expired")
	}
	return nil
}

func validateActionGrantClaims(c ActionGrantClaims) error {
	if c.Version != ActionGrantVersion {
		return fmt.Errorf("unsupported action grant version %d", c.Version)
	}
	for _, v := range []string{c.ClusterID, c.NodeUUID, c.SessionUUID, c.ConnectionID, c.ActionFamily, c.ResourceID, c.KeyID, c.GrantID} {
		if !validActionGrantText(v) {
			return errors.New("action grant has empty or invalid identity")
		}
	}
	if c.IssuerMemberID == 0 || c.SchedulingGeneration == 0 || c.OwnerMemberID == 0 || c.SessionGeneration == 0 {
		return errors.New("action grant has zero member or generation")
	}
	if len(c.PayloadSHA256) != 64 || strings.ToLower(c.PayloadSHA256) != c.PayloadSHA256 {
		return errors.New("invalid action grant payload digest")
	}
	if _, err := hex.DecodeString(c.PayloadSHA256); err != nil {
		return errors.New("invalid action grant payload digest")
	}
	if c.ActionFamily == "instance.run" || c.ActionFamily == "instance.stop" || c.TargetSHA256 != "" {
		if len(c.TargetSHA256) != 64 || strings.ToLower(c.TargetSHA256) != c.TargetSHA256 {
			return errors.New("invalid action grant target digest")
		}
		if _, err := hex.DecodeString(c.TargetSHA256); err != nil {
			return errors.New("invalid action grant target digest")
		}
	}
	if c.ActionFamily == "instance.remove" && c.TargetSHA256 != "" {
		return errors.New("removal action has no instance target digest")
	}
	if c.IssuedAtUnixMS <= 0 || c.NotAfterUnixMS <= c.IssuedAtUnixMS || c.NotAfterUnixMS-c.IssuedAtUnixMS > ActionGrantMaxTTL.Milliseconds() {
		return errors.New("invalid action grant lifetime")
	}
	return nil
}

func validActionGrantText(v string) bool {
	return v != "" && len(v) <= 1024 && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}

func actionGrantSigningBytes(c ActionGrantClaims) []byte {
	b := []byte(actionGrantDomain)
	b = append(b, c.Version)
	appendText := func(v string) { b = binary.AppendUvarint(b, uint64(len(v))); b = append(b, v...) }
	appendNumber := func(v uint64) { b = binary.BigEndian.AppendUint64(b, v) }
	appendText(c.ClusterID)
	appendText(c.NodeUUID)
	appendNumber(c.IssuerMemberID)
	appendNumber(c.SchedulingGeneration)
	appendNumber(c.OwnerMemberID)
	appendNumber(c.SessionGeneration)
	appendText(c.SessionUUID)
	appendText(c.ConnectionID)
	appendText(c.ActionFamily)
	appendText(c.ResourceID)
	appendNumber(c.ResourceVersion)
	appendText(c.PayloadSHA256)
	appendText(c.TargetSHA256)
	appendText(c.KeyID)
	appendText(c.GrantID)
	appendNumber(uint64(c.IssuedAtUnixMS))
	appendNumber(uint64(c.NotAfterUnixMS))
	return b
}

// UnmarshalJSON rejects unknown wire fields, including fields nested in the
// scope. Ordinary json.Unmarshal therefore cannot silently ignore a changed
// grant contract during a mixed-version rollout.
func (g *ActionGrant) UnmarshalJSON(data []byte) error {
	type wire ActionGrant
	var decoded wire
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&decoded); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("trailing action grant JSON")
		}
		return err
	}
	*g = ActionGrant(decoded)
	return nil
}
