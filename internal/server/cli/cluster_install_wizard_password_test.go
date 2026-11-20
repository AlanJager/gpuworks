package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadClusterWizardPasswordPreservesSpacesAndEditsUnicode(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"spaces", "  significant spaces  \n", "  significant spaces  "},
		{"tabs", "\tpassword\t\n", "\tpassword\t"},
		{"carriage return", "password\r", "password"},
		{"unicode", "密码🙂ab\n", "密码🙂ab"},
		{"backspace", "abc\bD\n", "abD"},
		{"delete", "abc\x7fD\n", "abD"},
		{"UTF8 backspace", "密码🙂\bX\n", "密码X"},
		{"UTF8 delete", "密码🙂\x7f\x7fX\n", "密X"},
		{"empty backspace", "\b\x7fpassword\n", "password"},
		{"clear line", "discard🙂\x15fresh password\n", "fresh password"},
		{"empty line", "\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readClusterWizardPassword(strings.NewReader(tc.input))
			if err != nil || got != tc.want {
				t.Fatalf("password input behavior differs: error=%v, got length=%d, want length=%d", err, len(got), len(tc.want))
			}
		})
	}
}

func TestReadClusterWizardPasswordCancelsAndRejectsTruncatedInput(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        error
	}{
		{"Ctrl-C", "partial secret\x03", context.Canceled},
		{"Ctrl-D", "partial secret\x04", io.ErrUnexpectedEOF},
		{"empty EOF", "", io.ErrUnexpectedEOF},
		{"partial EOF", "partial secret", io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readClusterWizardPassword(strings.NewReader(tc.input))
			if !errors.Is(err, tc.want) || got != "" {
				t.Fatalf("error=%v, want %v and no partial password", err, tc.want)
			}
		})
	}
	if got, err := readClusterWizardPassword(nil); got != "" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("nil input: error=%v, want unexpected EOF", err)
	}
}

func TestReadClusterWizardPasswordLimitsBytesAndPreservesNextLine(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		valid          bool
	}{
		{"4096 ASCII bytes", strings.Repeat("s", 4096), true},
		{"4097 ASCII bytes", strings.Repeat("s", 4097), false},
		{"4096 UTF8 bytes", strings.Repeat("🙂", 1024), true},
		{"4100 UTF8 bytes", strings.Repeat("🙂", 1025), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.NewReader(tc.password + "\nsecond-password\n")
			got, err := readClusterWizardPassword(input)
			if !tc.valid {
				if err == nil || got != "" || strings.Contains(err.Error(), tc.password) {
					t.Fatal("oversized password accepted or returned in an error")
				}
				return
			}
			if err != nil || got != tc.password {
				t.Fatalf("boundary password not accepted intact: %v", err)
			}
			next, err := readClusterWizardPassword(input)
			if err != nil || next != "second-password" {
				t.Fatalf("first read consumed the next password line: %v", err)
			}
		})
	}
}

func TestClusterInstallWizardPasswordSpacesRoundTrip(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	dir := t.TempDir()
	path, secretPath := filepath.Join(dir, "cluster.yaml"), filepath.Join(dir, "cluster-admin.password")
	secret := "  secret with significant spaces  "
	streams, out := clusterWizardIO(append(clusterWizardDefaults(), "y", "3", "", secret, secret)...)
	calls := 0
	err := runClusterInstallWizard(t.Context(), []string{"--interactive", "--config", path, "--binary", "/tmp/gpuworks-ha"}, streams, func(_ context.Context, args []string, check bool) error {
		calls++
		if check || !reflect.DeepEqual(args, []string{"--config", path, "--timeout", "15m0s", "--binary", "/tmp/gpuworks-ha", "--password-file", secretPath}) {
			t.Fatalf("only config and password paths should be forwarded: %q", args)
		}
		var options clusterInstallOptions
		if err := readClusterInstallPassword(secretPath, &options); err != nil || options.Password != secret {
			t.Fatalf("password-file reader changed whitespace: %v", err)
		}
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("wizard: %v, callbacks=%d", err, calls)
	}
	raw, err := os.ReadFile(secretPath)
	if err != nil || string(raw) != secret {
		t.Fatalf("saved password bytes changed: %v", err)
	}
	config, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), strings.TrimSpace(secret)) || strings.Contains(out.String(), strings.TrimSpace(secret)) {
		t.Fatal("significant-space password leaked into YAML or output")
	}
}
