package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/storagerules"
)

// A storage definition the agent would refuse to mount is refused when it is
// created or probed, including what the server's own older checks let through.
func TestValidateStorageRefusesWhatTheAgentRefusesToMount(t *testing.T) {
	nfs := func(export, options string) *store.Storage {
		return &store.Storage{Name: "nfs1", Type: proto.StorageNFS, Export: export, MountOptions: options, Path: "/mnt/gpuworks/nfs1"}
	}
	for _, tc := range []struct {
		name    string
		row     *store.Storage
		wantErr string // "" = accepted
	}{
		{"a plain definition", nfs("nfs.example.internal:/export/models", "vers=4.2,rsize=1048576"), ""},
		{"no options", nfs("10.0.0.5:/export", ""), ""},
		{"an ipv6 host", nfs("[fe80::1]:/srv", "rw,noatime"), ""},
		{"a shell metacharacter in the options", nfs("h:/x", "rw;reboot"), ErrorStorageOptionsInvalid},
		{"a space in the options", nfs("h:/x", "rw noatime"), ErrorStorageOptionsInvalid},
		// Accepted by the old server check, refused by the agent at mount time.
		{"an option the agent does not allow", nfs("h:/x", "rw,suid"), ErrorStorageOptionsInvalid},
		{"an unknown option", nfs("h:/x", "bogus=1"), ErrorStorageOptionsInvalid},
		{"an empty value", nfs("h:/x", "vers="), ErrorStorageOptionsInvalid},
		{"an empty item", nfs("h:/x", "rw,,noatime"), ErrorStorageOptionsInvalid},
		{"ro and rw", nfs("h:/x", "ro,rw"), ErrorStorageOptionsInvalid},
		{"conflicting values", nfs("h:/x", "vers=3,vers=4"), ErrorStorageOptionsInvalid},
		{"a control character in the options", nfs("h:/x", "rw,sec=sys\nnoatime"), ErrorStorageOptionsInvalid},
		{"an export without a host", nfs("/export", ""), ErrorStorageExportInvalid},
		{"an export without a path", nfs("no-colon", ""), ErrorStorageExportInvalid},
		{"an export that is not absolute", nfs("host:export", ""), ErrorStorageExportInvalid},
		{"an export that looks like an option", nfs("-o:/x", ""), ErrorStorageExportInvalid},
		{"a control character in the export", nfs("host:/a\x7f", ""), ErrorStorageExportInvalid},
		{"a shell metacharacter in the export", nfs("host:/a;reboot", ""), ErrorStorageExportInvalid},
	} {
		err := validateStorage(tc.row)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted, want %s", tc.name, tc.wantErr)
			continue
		}
		if got := productErrorCode(t, err); got != tc.wantErr {
			t.Errorf("%s: code = %s, want %s", tc.name, got, tc.wantErr)
		}
		// Whatever the server refuses here is, by construction, what the rule
		// both ends share refuses.
		if _, optErr := storagerules.ValidateMountOptions(tc.row.MountOptions); tc.wantErr == ErrorStorageOptionsInvalid && optErr == nil {
			t.Errorf("%s: the shared rule accepts the options", tc.name)
		}
		if exportErr := storagerules.ValidateExport(tc.row.Export); tc.wantErr == ErrorStorageExportInvalid && exportErr == nil {
			t.Errorf("%s: the shared rule accepts the export", tc.name)
		}
	}
}

// The probe refuses such a definition itself instead of sending it to a node.
func TestProbeStorageRefusesAnUnmountableDefinitionBeforeCallingANode(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu3", store.HashToken("b"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := &apiHandlers{d: Deps{Store: st, Online: func(uuid string) bool { return uuid == node.UUID },
		CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
			calls++
			return json.Marshal(proto.StorageProbeResult{OK: true})
		}}}
	probe := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		h.probeStorage(rec, httptest.NewRequest(http.MethodPost, "/api/v1/storages/probe", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	for body, code := range map[string]string{
		`{"type":"nfs","path":"/mnt/x","export":"h:/x","mount_options":"rw,suid"}`: ErrorStorageOptionsInvalid,
		`{"type":"nfs","path":"/mnt/x","export":"host:path"}`:                      ErrorStorageExportInvalid,
	} {
		if status, out := probe(body); status != http.StatusBadRequest || !strings.Contains(out, `"code":"`+code+`"`) {
			t.Errorf("probe %s = %d %s, want 400 %s", body, status, out, code)
		}
	}
	if calls != 0 {
		t.Fatalf("a refused definition reached a node %d times", calls)
	}
	if status, out := probe(`{"type":"nfs","path":"/mnt/x","export":"h:/x","mount_options":"vers=4.2"}`); status != http.StatusOK || calls != 1 {
		t.Fatalf("a valid definition = %d %s after %d calls", status, out, calls)
	}
}
