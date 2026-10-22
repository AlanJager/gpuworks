// Package installcred owns the bootstrap admin credential record that
// `gpuworks install` writes and the server reads on first start. It is a leaf:
// both sides of that handoff import it, so it must not import the server.
package installcred

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// initialCredentialsFile records the bootstrap admin password in the data dir.
//
// It exists so `gpuworks install` can hand a password to the first start of a
// service it cannot talk to yet (the unit file must never carry a password),
// and so an operator can read a generated one after the fact. The server keeps
// the file after the admin user is created; it holds the password in clear
// text, so it must be deleted once the password has been changed.
const initialCredentialsFile = "initial-credentials"

// InitialCredentials is the on-disk record written when the admin user is
// created. CreatedAt is informational (RFC 3339) and never parsed back.
type InitialCredentials struct {
	User      string `json:"user"`
	Password  string `json:"password"`
	CreatedAt string `json:"created_at"`
}

// CredentialsPath returns the location of the bootstrap credentials file for a
// data dir. install and the server bootstrap must agree on it.
func CredentialsPath(dataDir string) string {
	return filepath.Join(dataDir, initialCredentialsFile)
}

// InitialCredentialsPayload renders the record WriteInitialCredentials stores,
// so callers that write through their own privileged path produce the same
// bytes.
func InitialCredentialsPayload(user, password string) ([]byte, error) {
	if user == "" || password == "" {
		return nil, errors.New("initial credentials require both user and password")
	}
	payload, err := json.MarshalIndent(InitialCredentials{
		User:      user,
		Password:  password,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

// WriteInitialCredentials stores the bootstrap credentials with 0664. The write
// goes through a temporary file so a crash cannot leave a half-written record
// that a later start would misread as "no password configured".
func WriteInitialCredentials(dataDir, user, password string) error {
	if _, err := InitialCredentialsPayload(user, password); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	payload, err := InitialCredentialsPayload(user, password)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, initialCredentialsFile+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	// 0664 on purpose: written by the operator, read by the server's first
	// boot under the service account, group-writable like the rest of the tree.
	if err := tmp.Chmod(0o664); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, CredentialsPath(dataDir))
}

// ReadInitialCredentials returns the recorded credentials. A missing file is
// reported as an error; callers treat every error the same way (fall back to
// generating a password), so a truncated or hand-edited file cannot block
// startup.
func ReadInitialCredentials(dataDir string) (InitialCredentials, error) {
	path := CredentialsPath(dataDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return InitialCredentials{}, err
	}
	var creds InitialCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return InitialCredentials{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if creds.Password == "" {
		return InitialCredentials{}, fmt.Errorf("%s carries no password", path)
	}
	return creds, nil
}
