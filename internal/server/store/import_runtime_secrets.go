//go:build linux && dqlite

package store

import (
	"context"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/auth"
)

// validateImportedSecrets checks the effective service key before the source
// is archived. Prefix-bearing columns retain their existing plaintext legacy
// compatibility; cloud/SSH credentials are always encrypted.
func (s *Store) validateImportedSecrets(ctx context.Context, masterSecret string) error {
	box, err := auth.NewSecretbox(masterSecret)
	if err != nil {
		return err
	}
	for _, field := range []struct {
		model    any
		column   string
		prefixed bool
	}{
		{&Provider{}, "creds_cipher", false},
		{&NodeSSHCredential{}, "creds_cipher", false},
		{&AIConfig{}, "api_key", true},
		{&BenchmarkRun{}, "api_key", true},
	} {
		rows, err := s.db.WithContext(ctx).Model(field.model).Select(field.column).Rows()
		if err != nil {
			return fmt.Errorf("inspect imported credential field %T.%s: %w", field.model, field.column, err)
		}
		checkErr := func() error {
			defer rows.Close()
			for rows.Next() {
				var value *string
				if err := rows.Scan(&value); err != nil {
					return err
				}
				if value == nil || *value == "" {
					continue
				}
				cipher := *value
				if field.prefixed {
					if !strings.HasPrefix(cipher, encryptedAIKeyPrefix) {
						continue
					}
					cipher = strings.TrimPrefix(cipher, encryptedAIKeyPrefix)
				}
				if _, err := box.Decrypt(cipher); err != nil {
					// Do not include stored ciphertext or plaintext in operational output.
					return fmt.Errorf("effective master secret cannot decrypt %T.%s; use the original service secret before activating the import", field.model, field.column)
				}
			}
			return rows.Err()
		}()
		if checkErr != nil {
			return checkErr
		}
	}
	return nil
}
