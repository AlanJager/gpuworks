package runpod

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// Sync removes rows for pods the listing omits, so an unrecognised body must
// be an error rather than "no pods".
func TestListInstancesEnvelopesAndUnknownShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantIDs []string
		wantErr bool
	}{
		{name: "bare empty array", body: `[]`, wantIDs: []string{}},
		{name: "pods envelope", body: `{"pods":[{"id":"p1"}]}`, wantIDs: []string{"p1"}},
		{name: "data envelope", body: `{"data":[{"id":"p2"}]}`, wantIDs: []string{"p2"}},
		{name: "empty pods envelope", body: `{"pods":[]}`, wantIDs: []string{}},
		{name: "unknown envelope", body: `{"items":[{"id":"p3"}]}`, wantErr: true},
		{name: "error object with 200", body: `{"error":"rate limited"}`, wantErr: true},
		{name: "null pods", body: `{"pods":null}`, wantErr: true},
		{name: "null body", body: `null`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			insts, err := d.ListInstances(context.Background(), provider.Credentials{APIKey: "k"})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ListInstances(%s) = %+v, want error", tc.body, insts)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListInstances(%s): %v", tc.body, err)
			}
			ids := make([]string, 0, len(insts))
			for _, inst := range insts {
				ids = append(ids, inst.ExternalID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}
