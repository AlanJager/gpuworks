package provision

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// PlatformKey is the server's own SSH identity: one key pair the server logs
// in to nodes with, so operators authorize its public key once instead of
// handing the server a private key or password per node. See
// docs/how-to/connect-nodes.md.
type PlatformKey struct {
	PrivatePEM  string // unencrypted OpenSSH private key
	PublicKey   string // one authorized_keys line, with comment
	Fingerprint string // SHA256:...
}

// GeneratePlatformKey creates a new ed25519 platform key. comment ends up in
// authorized_keys so an operator can tell which server a line belongs to.
func GeneratePlatformKey(comment string) (PlatformKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return PlatformKey{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return PlatformKey{}, err
	}
	return ParsePlatformKey(string(pem.EncodeToMemory(block)), comment)
}

// ParsePlatformKey validates an imported private key and derives its public
// half. Passphrase-protected keys are refused: the server has to use the key
// unattended.
func ParsePlatformKey(privatePEM, comment string) (PlatformKey, error) {
	privatePEM = strings.TrimSpace(privatePEM)
	if privatePEM == "" {
		return PlatformKey{}, errors.New("private key is required")
	}
	signer, err := ssh.ParsePrivateKey([]byte(privatePEM))
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return PlatformKey{}, errors.New("passphrase-protected private keys are not supported")
		}
		return PlatformKey{}, fmt.Errorf("parse private key: %w", err)
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment = strings.TrimSpace(comment); comment != "" {
		public += " " + comment
	}
	return PlatformKey{
		PrivatePEM:  privatePEM + "\n",
		PublicKey:   public,
		Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
	}, nil
}

// PlatformKeyComment is the authorized_keys comment for a server host.
func PlatformKeyComment(hostname string) string {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return "gpuworks"
	}
	return "gpuworks@" + hostname
}
