package cluster

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// ValidateMaintenanceCheckpoint checks the complete current-format inventory,
// aggregate digest and three-voter proof shape. It authenticates no archive or
// process: cold restore must independently reproduce these raw SQL digests.
func ValidateMaintenanceCheckpoint(checkpoint MaintenanceCheckpoint) error {
	if checkpoint.Version != 1 || checkpoint.SchemaVersion != 136 || checkpoint.Phase.Phase != "drained" || checkpoint.Phase.Revision <= 0 || !maintenanceDigest(checkpoint.SchemaDigest, 32) || !maintenanceDigest(checkpoint.Digest, 32) || len(checkpoint.Tables) != len(maintenanceCheckpointColumns) {
		return errors.New("invalid maintenance checkpoint format or schema")
	}
	if err := validateMaintenanceQuorumProof(checkpoint.Proof); err != nil {
		return err
	}
	var names []string
	for name := range maintenanceCheckpointColumns {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, table := range checkpoint.Tables {
		if table.Name != names[i] || !maintenanceDigest(table.SHA256, 32) {
			return errors.New("incomplete or ambiguous maintenance checkpoint table inventory")
		}
	}
	if checkpoint.Digest != MaintenanceCheckpointDigest(checkpoint.SchemaDigest, checkpoint.Tables) {
		return errors.New("maintenance checkpoint aggregate digest mismatch")
	}
	return nil
}

func validateMaintenanceQuorumProof(proof QuorumProof) error {
	if proof.Term == 0 || proof.Index == 0 || proof.ConfigurationIndex == 0 || proof.ConfigurationIndex > proof.Index || proof.LeaderID == 0 || len(proof.Members) != 3 {
		return errors.New("checkpoint requires an original three-voter quorum proof")
	}
	seenID := map[uint64]bool{}
	seenAddress := map[string]bool{}
	leader := false
	for _, member := range proof.Members {
		normalized, err := identity.NormalizeMemberAddress(member.Address)
		if err != nil || normalized != member.Address || member.ID == 0 || member.Role != "voter" || seenID[member.ID] || seenAddress[member.Address] {
			return errors.New("invalid checkpoint native voter configuration")
		}
		seenID[member.ID] = true
		seenAddress[member.Address] = true
		leader = leader || member.ID == proof.LeaderID
	}
	if !leader {
		return errors.New("checkpoint leader is not a voter")
	}
	return nil
}

// MaintenanceCheckpointDigest preserves the canonical format hash. It grants no authority.
func MaintenanceCheckpointDigest(schemaDigest string, tables []MaintenanceTableDigest) string {
	hash := sha256.New()
	hash.Write([]byte("gpuworks-maintenance-checkpoint-v1\x00"))
	maintenanceWriteSized(hash, schemaDigest)
	for _, table := range tables {
		maintenanceWriteSized(hash, table.Name)
		var count [8]byte
		binary.BigEndian.PutUint64(count[:], table.Rows)
		hash.Write(count[:])
		maintenanceWriteSized(hash, table.SHA256)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type maintenanceCheckpointHashWriter interface{ Write([]byte) (int, error) }

func maintenanceWriteSized(writer maintenanceCheckpointHashWriter, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	// A hash never fails a write.
	_, _ = writer.Write(length[:])
	_, _ = writer.Write([]byte(value))
}
