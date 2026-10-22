package docker

import (
	"context"
	"errors"
	"testing"
)

func TestImageExistsDistinguishesMissingFromRuntimeFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body       string
		present, failure bool
	}{
		{"present", "exit 0", true, false},
		{"missing", "echo 'Error: No such image: org/model:v1' >&2; exit 1", false, false},
		{"daemon", "echo 'cannot connect to runtime daemon' >&2; exit 1", false, true},
		{"empty failure", "exit 1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeDocker(t, tc.body)
			got, err := New().ImageExists(context.Background(), "org/model:v1")
			if got != tc.present || (err != nil) != tc.failure {
				t.Fatalf("exists=%v err=%v", got, err)
			}
		})
	}
	t.Run("canceled", func(t *testing.T) {
		installFakeDocker(t, "exit 0")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := New().ImageExists(ctx, "org/model:v1")
		if got || !errors.Is(err, context.Canceled) {
			t.Fatalf("exists=%v err=%v", got, err)
		}
	})
}
