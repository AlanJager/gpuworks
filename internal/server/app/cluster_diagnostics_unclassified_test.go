package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/diagnostics"
)

// A single server's collection made without the server, on a data directory
// whose cluster markers cannot be read: the command says on the terminal
// that it cannot tell what the directory is, and the archive carries the
// same as a failed item, with the role records its log holds beside the
// log. Every build does this; a directory whose markers can be read is left
// as it was.
func TestAnOfflineCollectionSaysWhenTheDirectoryCannotBeClassified(t *testing.T) {
	collect := func(t *testing.T, data string) diagnostics.Bundle {
		t.Helper()
		logFile := filepath.Join(data, "server.log")
		at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		if err := os.WriteFile(logFile, []byte("time="+at+` level=WARN msg="cluster database role event" event=loop_late late_ms=1819`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		bundle, _, err := CollectDiagnosticsOffline(t.Context(), OfflineDiagnosticsRequest{DataDir: data, DBPath: filepath.Join(data, "gpuworks.db"), LogFile: logFile, WorkDir: t.TempDir(),
			Request: diagnostics.BundleRequest{RequestedBy: "support-bundle"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil || bundle.State != "ready" {
			t.Fatalf("collection = %q, %v", bundle.State, err)
		}
		return bundle
	}
	items := func(b diagnostics.Bundle) map[string]diagnostics.BundleItem {
		out := map[string]diagnostics.BundleItem{}
		for _, it := range b.Items {
			out[it.Path] = it
		}
		return out
	}
	t.Run("markers cannot be read", func(t *testing.T) {
		data := t.TempDir()
		// A file where the enrollment directory belongs: looking inside it
		// fails with something other than "does not exist".
		marker := filepath.Join(data, "cluster-enrollment")
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		got := items(collect(t, data))
		it := got["members"]
		if it.Status != proto.CheckFail || it.ID() != diagnostics.BundleItemMembers ||
			!strings.HasPrefix(it.Evidence, "whether this installation is a control-plane cluster member could not be read: ") || !strings.Contains(it.Evidence, marker) {
			t.Fatalf("members item = %+v", it)
		}
		if records := got["server/role-events.log"]; records.Status != proto.CheckPass || records.Bytes == 0 {
			t.Fatalf("role records beside the log = %+v", records)
		}
	})
	t.Run("a single server", func(t *testing.T) {
		got := items(collect(t, t.TempDir()))
		for path := range got {
			if path == "members" || path == "server/role-events.log" || path == "server/member.json" {
				t.Fatalf("a single server's collection has %s", path)
			}
		}
		if got["server/gpuworks-server.log"].Status != proto.CheckPass {
			t.Fatalf("items = %+v", got)
		}
	})
}
