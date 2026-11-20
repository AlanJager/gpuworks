package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSingleDatabaseRefusesAmbiguousIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, info, members, want string
	}{
		{"missing-identity", "", "- ID: 1\n  Address: 127.0.0.1:19000\n", "identity is missing"},
		{"multiple-members", "ID: 1\nAddress: 127.0.0.1:19000\n", "- ID: 1\n  Address: 127.0.0.1:19000\n- ID: 2\n  Address: 127.0.0.1:19001\n", "not an established single-member"},
		{"different-member", "ID: 1\nAddress: 127.0.0.1:19000\n", "- ID: 2\n  Address: 127.0.0.1:19000\n", "not an established single-member"},
		{"missing-membership", "ID: 1\nAddress: 127.0.0.1:19000\n", "", "read database membership"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range map[string]string{"info.yaml": tc.info, "cluster.yaml": tc.members} {
				if content != "" {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := OpenSingleDatabase(t.Context(), dir, "gpuworks.db"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}
