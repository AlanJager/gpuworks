package cluster

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// MaintenanceDrainBody records an actual process owner's completed drain.
// Signatures authenticate observations; admission, freshness and quorum must
// still be checked against the same operation's native snapshot.
type MaintenanceDrainBody struct {
	Version              int                              `json:"version"`
	RunID                string                           `json:"run_id"`
	ClusterID            string                           `json:"cluster_id"`
	MemberID             uint64                           `json:"member_id"`
	MemberKey            string                           `json:"member_key"`
	InstallDir           string                           `json:"install_dir"`
	BinarySHA256         string                           `json:"binary_sha256"`
	PID                  int                              `json:"pid"`
	BootID               string                           `json:"boot_id"`
	ProcessStart         string                           `json:"process_start"`
	ServingRevision      int64                            `json:"serving_revision"`
	FreezingRevision     int64                            `json:"freezing_revision"`
	SchedulingGeneration uint64                           `json:"scheduling_generation"`
	AgentDrains          []proto.ActionDrainInspectResult `json:"agent_drains"`
}

type MaintenanceDrainReceipt struct {
	Body           MaintenanceDrainBody `json:"body"`
	CertificateDER []byte               `json:"certificate_der"`
	Signature      []byte               `json:"signature"`
}

var ErrMaintenanceDrainReceipt = errors.New("maintenance drain receipt is invalid")

func maintenanceDigest(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && strings.ToLower(value) == value
}

func (body MaintenanceDrainBody) validate() error {
	if body.Version != 1 || len(body.RunID) == 0 || len(body.RunID) > 128 ||
		!maintenanceDigest(body.ClusterID, 16) || !maintenanceDigest(body.MemberKey, 32) || !maintenanceDigest(body.BinarySHA256, 32) ||
		body.MemberID == 0 || !filepath.IsAbs(body.InstallDir) || filepath.Clean(body.InstallDir) != body.InstallDir || body.InstallDir == "/" ||
		body.PID <= 1 || body.ServingRevision <= 0 || body.ServingRevision == math.MaxInt64 || body.FreezingRevision != body.ServingRevision+1 ||
		body.SchedulingGeneration == 0 || body.SchedulingGeneration == math.MaxUint64 {
		return ErrMaintenanceDrainReceipt
	}
	for i, c := range body.RunID {
		alphanumeric := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !(alphanumeric || i > 0 && (c == '-' || c == '_' || c == '.')) {
			return ErrMaintenanceDrainReceipt
		}
	}
	if len(body.BootID) != 36 || body.BootID[8] != '-' || body.BootID[13] != '-' || body.BootID[18] != '-' || body.BootID[23] != '-' || !maintenanceDigest(strings.ReplaceAll(body.BootID, "-", ""), 16) {
		return ErrMaintenanceDrainReceipt
	}
	start, err := strconv.ParseUint(body.ProcessStart, 10, 64)
	if err != nil || start == 0 || strconv.FormatUint(start, 10) != body.ProcessStart {
		return ErrMaintenanceDrainReceipt
	}
	previous := ""
	for _, proof := range body.AgentDrains {
		current := proof.NodeUUID + "\x00" + proof.SessionUUID
		if proof.NodeUUID == "" || proof.SessionUUID == "" || strings.ContainsRune(proof.NodeUUID, 0) || strings.ContainsRune(proof.SessionUUID, 0) ||
			proof.ConnectionID == "" || proof.SessionGeneration == 0 || proof.ThroughGeneration != body.SchedulingGeneration ||
			proof.PendingCalls != 0 || proof.ObservedAtUnixMS <= 0 || current <= previous {
			return ErrMaintenanceDrainReceipt
		}
		previous = current
	}
	return nil
}

func maintenanceReceiptMessage(body MaintenanceDrainBody, domain string) ([]byte, error) {
	if err := body.validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return append([]byte(domain+"\x00"), encoded...), nil
}

// SignMaintenanceDrainReceipt requires the original encoded member certificate
// and matching private key. Callers must only use it after real owner completion.
func SignMaintenanceDrainReceipt(body MaintenanceDrainBody, certificate tls.Certificate) (MaintenanceDrainReceipt, error) {
	return signMaintenanceReceipt(body, certificate, "gpuworks-maintenance-drain-v1")
}

func signMaintenanceReceipt(body MaintenanceDrainBody, certificate tls.Certificate, domain string) (MaintenanceDrainReceipt, error) {
	message, err := maintenanceReceiptMessage(body, domain)
	if err != nil {
		return MaintenanceDrainReceipt{}, err
	}
	if len(certificate.Certificate) == 0 {
		return MaintenanceDrainReceipt{}, ErrMaintenanceDrainReceipt
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return MaintenanceDrainReceipt{}, ErrMaintenanceDrainReceipt
	}
	public, ok := leaf.PublicKey.(ed25519.PublicKey)
	private, privateOK := certificate.PrivateKey.(ed25519.PrivateKey)
	fingerprint := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	if !ok || !privateOK || len(private) != ed25519.PrivateKeySize || !public.Equal(private.Public()) || hex.EncodeToString(fingerprint[:]) != body.MemberKey {
		return MaintenanceDrainReceipt{}, ErrMaintenanceDrainReceipt
	}
	body.AgentDrains = append(body.AgentDrains[:0:0], body.AgentDrains...)
	return MaintenanceDrainReceipt{Body: body, CertificateDER: append([]byte(nil), leaf.Raw...), Signature: ed25519.Sign(private, message)}, nil
}

// VerifyMaintenanceDrainReceipt verifies only pinned cryptographic identity.
// A valid signature alone does not prove that the key is still admitted.
func VerifyMaintenanceDrainReceipt(receipt MaintenanceDrainReceipt, trust *identity.Trust, now time.Time) (MaintenanceDrainBody, error) {
	return verifyMaintenanceReceipt(receipt, trust, now, "gpuworks-maintenance-drain-v1")
}

func verifyMaintenanceReceipt(receipt MaintenanceDrainReceipt, trust *identity.Trust, now time.Time, domain string) (MaintenanceDrainBody, error) {
	if trust == nil || now.IsZero() || len(receipt.CertificateDER) > 65536 || len(receipt.Signature) != ed25519.SignatureSize {
		return MaintenanceDrainBody{}, ErrMaintenanceDrainReceipt
	}
	message, err := maintenanceReceiptMessage(receipt.Body, domain)
	if err != nil {
		return MaintenanceDrainBody{}, err
	}
	leaf, err := x509.ParseCertificate(receipt.CertificateDER)
	if err != nil {
		return MaintenanceDrainBody{}, ErrMaintenanceDrainReceipt
	}
	key, err := trust.VerifyMember(leaf, now)
	if err != nil || key != receipt.Body.MemberKey {
		return MaintenanceDrainBody{}, ErrMaintenanceDrainReceipt
	}
	// VerifyMember also binds the exact cluster identity URI.
	if len(leaf.URIs) != 1 || leaf.URIs[0].Path != "/cluster/"+receipt.Body.ClusterID+"/member/"+key {
		return MaintenanceDrainBody{}, ErrMaintenanceDrainReceipt
	}
	public, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !ed25519.Verify(public, message, receipt.Signature) {
		return MaintenanceDrainBody{}, ErrMaintenanceDrainReceipt
	}
	body := receipt.Body
	body.AgentDrains = append(body.AgentDrains[:0:0], body.AgentDrains...)
	return body, nil
}

// MaintenancePreparationReceipt authenticates a durable local hold and route
// withdrawal only. Its separate signature domain cannot be used as a drain.
type MaintenancePreparationReceipt MaintenanceDrainReceipt

func SignMaintenancePreparationReceipt(body MaintenanceDrainBody, certificate tls.Certificate) (MaintenancePreparationReceipt, error) {
	if len(body.AgentDrains) != 0 {
		return MaintenancePreparationReceipt{}, ErrMaintenanceDrainReceipt
	}
	receipt, err := signMaintenanceReceipt(body, certificate, "gpuworks-maintenance-prepared-v1")
	return MaintenancePreparationReceipt(receipt), err
}

func VerifyMaintenancePreparationReceipt(receipt MaintenancePreparationReceipt, trust *identity.Trust, now time.Time) (MaintenanceDrainBody, error) {
	if len(receipt.Body.AgentDrains) != 0 {
		return MaintenanceDrainBody{}, ErrMaintenanceDrainReceipt
	}
	return verifyMaintenanceReceipt(MaintenanceDrainReceipt(receipt), trust, now, "gpuworks-maintenance-prepared-v1")
}
