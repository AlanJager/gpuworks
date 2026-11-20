//go:build linux && dqlite

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterAddressRejectsInvalidArgumentsBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"online", []string{"--address", "127.0.0.1:19000"}, "--offline"},
		{"missing address", []string{"--offline"}, "--address"},
		{"positional", []string{"--offline", "unexpected"}, "positional"},
		{"timeout", []string{"--offline", "--address", "127.0.0.1:19000", "--timeout", "0s"}, "positive timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "absent")
			args := append([]string{"--data", dir}, tc.args...)
			err := clusterAddressCommand(context.Background(), args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v want %s", err, tc.want)
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatalf("invalid arguments modified filesystem: %v", err)
			}
		})
	}
}
