package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

// clusterInstallExecutable is one local file the installer copies into every
// member's installation directory under a fixed name.
type clusterInstallExecutable struct {
	Name    string
	Payload []byte
	Digest  string
}

func readClusterInstallExecutable(path, flagName string) (clusterInstallExecutable, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return clusterInstallExecutable{}, err
	}
	if len(payload) < 20 || !bytes.Equal(payload[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return clusterInstallExecutable{}, fmt.Errorf("%s must be a Linux ELF executable", flagName)
	}
	digest := sha256.Sum256(payload)
	return clusterInstallExecutable{Payload: payload, Digest: hex.EncodeToString(digest[:])}, nil
}

// clusterInstallExecutableCommand never replaces an existing file, and fails
// the transfer when the stored bytes differ from the local digest.
func clusterInstallExecutableCommand(dir string, executable clusterInstallExecutable) string {
	target := dir + "/" + executable.Name
	return "set -C; cat > " + clusterInstallQuote(target) + "; chmod 700 " + clusterInstallQuote(target) + "; echo " + clusterInstallQuote(executable.Digest+"  "+target) + " | sha256sum -c - >/dev/null"
}
