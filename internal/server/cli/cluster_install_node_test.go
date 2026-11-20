package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestClusterInstallNodeAnswersCapabilitiesAndWritesNothing(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	raw, err := json.Marshal(clusterInstallNodeRequest{Action: "capabilities"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := clusterInstallNodeCommand(t.Context(), bytes.NewReader(raw), &output); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Native bool   `json:"native_dqlite"`
		OS     string `json:"os"`
		Arch   string `json:"arch"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Native != clusterInstallRuntimeAvailable() || got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Fatalf("capabilities = %+v", got)
	}
	prepare, err := json.Marshal(clusterInstallNodeRequest{Action: "prepare", Dir: work})
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	err = clusterInstallNodeCommand(t.Context(), bytes.NewReader(prepare), &output)
	// Without the runtime the refusal is fixed; with it, an account that is not
	// the request's must still stop the action before any write.
	want := "cluster-install requires a Linux native dqlite executable"
	if clusterInstallRuntimeAvailable() {
		want = "installation account differs from SSH account"
	}
	if err == nil || err.Error() != want || output.Len() != 0 {
		t.Fatalf("prepare err=%v output=%s", err, output.String())
	}
	entries, readErr := os.ReadDir(work)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("prepare wrote %v (%v)", entries, readErr)
	}
}

func TestClusterInstallNodeRejectsMalformedInput(t *testing.T) {
	cases := []string{
		`{"Action":"capabilities","Nope":1}`,
		`{"Action":"capabilities"}{"Action":"prepare"}`,
		`{"Action":"` + strings.Repeat("x", 70000) + `"}`,
	}
	for _, raw := range cases {
		err := clusterInstallNodeCommand(t.Context(), strings.NewReader(raw), io.Discard)
		if err == nil || (err.Error() != "invalid cluster installation request" && err.Error() != "unexpected cluster installation input") {
			t.Fatalf("input accepted: %v", err)
		}
		if errors.Is(err, io.EOF) {
			t.Fatal("eof leaked")
		}
	}
}
