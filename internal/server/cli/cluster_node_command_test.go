package cli

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestClusterMemberCommandsAreListedAndDispatched(t *testing.T) {
	for _, line := range []string{
		"  gpuworks cluster install --config cluster.yaml --binary HA_BINARY --database-helper HELPER [--password-file FILE]\n",
		"                           [--storage-tier standard|slow-storage] [--storage-probe 30s]\n",
		"  gpuworks cluster-init --offline --listen IP:PORT --public-url https://HOST:PORT [--storage-tier standard|slow-storage] [--data ./data]\n",
		"  gpuworks cluster-address --offline --address HOST:PORT [--data ./data]\n",
		"  gpuworks cluster-join --address HOST:PORT --cluster-id ID --ca-fingerprint FP --enrollment-url https://HOST:PORT [--data ./data]\n",
		"  gpuworks cluster-status [--data ./data] [--http-addr HOST:PORT] [--timeout 5s]\n",
	} {
		if !strings.Contains(clusterCommandUsage, line) {
			t.Fatalf("usage omits %q", line)
		}
	}
	// A positional argument is refused by every member command before it reads
	// or writes anything, in a build with the runtime and in one without it.
	for _, name := range []string{"cluster-init", "cluster-address", "cluster-join", "cluster-status"} {
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			t.Chdir(work)
			var output, logs bytes.Buffer
			code := clusterNodeExit(slog.New(slog.NewTextHandler(&logs, nil)), name, []string{"--data", work + "/data", "unexpected"}, &output)
			want := "does not accept positional arguments"
			if !clusterInstallRuntimeAvailable() {
				want = "this command requires a Linux native dqlite executable"
			}
			if code != 1 || output.Len() != 0 || !strings.Contains(logs.String(), `msg="`+name+` failed"`) || !strings.Contains(logs.String(), want) {
				t.Fatalf("exit=%d output=%q logs=%s", code, output.String(), logs.String())
			}
			entries, err := os.ReadDir(work)
			if err != nil || len(entries) != 0 {
				t.Fatalf("refused command wrote %v (%v)", entries, err)
			}
		})
	}
}

func TestClusterInstallSupervisorCommandRefusesArguments(t *testing.T) {
	var output, logs bytes.Buffer
	if code := run(slog.New(slog.NewTextHandler(&logs, nil)), []string{clusterInstallSupervisorCommand, "extra"}, &output); code != 2 || output.Len() != 0 || logs.Len() != 0 {
		t.Fatalf("exit=%d output=%q logs=%s", code, output.String(), logs.String())
	}
}

func TestClusterInstallNodeFailureLogOmitsCredentials(t *testing.T) {
	path := t.TempDir() + "/node-errors.log"
	req := clusterInstallNodeRequest{Action: "join", Password: "private-password", Invitation: []byte(`{"token":"private-token"}`)}
	clusterInstallLogNodeError(path, req, nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a successful action wrote a failure log: %v", err)
	}
	clusterInstallLogNodeError(path, req, errors.New("join failed: private-password and private-token"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-password") || strings.Contains(string(raw), "private-token") || info.Mode().Perm() != 0600 {
		t.Fatalf("failure log disclosed credentials or is not private: %s", raw)
	}
	if !strings.HasSuffix(string(raw), " join: join failed: [password omitted] and [invitation omitted]\n") {
		t.Fatalf("failure cause was lost: %q", raw)
	}
}
