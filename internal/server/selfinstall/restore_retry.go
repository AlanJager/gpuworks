package selfinstall

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

// restorePointLoss is the explicit statement attached to a completed
// single-member restore. Commits made after the snapshot are gone.
const restorePointLoss = "commits after this snapshot are absent from the restored member; this restore point is not majority failover and does not keep those commits"

// RestoredVoter is one voter in the cluster a restored member can grow back
// into. Planning these voters does not start them.
type RestoredVoter struct {
	ID      uint64 `json:"id"`
	Address string `json:"address"`
	Role    string `json:"role"`
}

// RestoreRetryResult is a completed retry. Loss is always the restore-point
// statement above. Voters is the restored member plus the two added voters.
type RestoreRetryResult struct {
	Loss   string          `json:"loss"`
	Voters []RestoredVoter `json:"voters"`
}

type archiveIdentity struct {
	sum  [32]byte
	mode os.FileMode
	mod  int64
	size int64
}

func readArchiveIdentity(path string) (archiveIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return archiveIdentity{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return archiveIdentity{}, err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return archiveIdentity{}, err
	}
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return archiveIdentity{sum: sum, mode: info.Mode(), mod: info.ModTime().UnixNano(), size: n}, nil
}

// RetryInterruptedRestore runs the restore again after an interrupted attempt
// has rolled back. The archive's bytes, mode, and modification time stay as
// they were. A still-pending restore is refused before the archive is restored.
func (i *Installer) RetryInterruptedRestore(archive string, restored backupNodeIdentity, added []backupNodeIdentity) (RestoreRetryResult, error) {
	if restored.ID == 0 || restored.Address == "" || len(added) != 2 {
		return RestoreRetryResult{}, errors.New("restored cluster needs the restored voter and two more voters")
	}
	voters := []RestoredVoter{{ID: restored.ID, Address: restored.Address, Role: "voter"}}
	seenID := map[uint64]struct{}{restored.ID: {}}
	seenAddress := map[string]struct{}{restored.Address: {}}
	for _, member := range added {
		if member.ID == 0 || member.Address == "" {
			return RestoreRetryResult{}, errors.New("each added voter needs an id and an address")
		}
		if _, ok := seenID[member.ID]; ok {
			return RestoreRetryResult{}, fmt.Errorf("voter id %d is repeated", member.ID)
		}
		if _, ok := seenAddress[member.Address]; ok {
			return RestoreRetryResult{}, fmt.Errorf("voter address %s is repeated", member.Address)
		}
		seenID[member.ID] = struct{}{}
		seenAddress[member.Address] = struct{}{}
		voters = append(voters, RestoredVoter{ID: member.ID, Address: member.Address, Role: "voter"})
	}
	before, err := readArchiveIdentity(archive)
	if err != nil {
		return RestoreRetryResult{}, err
	}
	unchanged := func() error {
		after, readErr := readArchiveIdentity(archive)
		if readErr != nil {
			return readErr
		}
		if after != before {
			return errors.New("interrupted restore retry changed the source archive")
		}
		return nil
	}
	if err := RestorePending(i.layout.DataDir); err != nil {
		return RestoreRetryResult{}, errors.Join(err, unchanged())
	}
	if err := i.restoreBackup(archive); err != nil {
		return RestoreRetryResult{}, errors.Join(err, unchanged())
	}
	if err := unchanged(); err != nil {
		return RestoreRetryResult{}, err
	}
	return RestoreRetryResult{Loss: restorePointLoss, Voters: voters}, nil
}
