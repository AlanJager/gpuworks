package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestClusterInstallRequiresBootstrapBeforeFormalBusiness(t *testing.T) {
	for _, fault := range []string{"", "missing-pointer", "consumer-incomplete", "unsupported", "malformed"} {
		t.Run(fault, func(t *testing.T) {
			o := offlineInstallOptions(t)
			services, bootstraps, formal := 0, 0, 0
			remote := func(ctx context.Context, host string, input io.Reader) ([]byte, error) {
				var req clusterInstallNodeRequest
				if err := json.NewDecoder(input).Decode(&req); err != nil {
					t.Fatal(err)
				}
				switch req.Action {
				case "capabilities":
					return []byte(`{"native_dqlite":true,"os":"linux"}`), nil
				case "seed":
					return []byte(`{"cluster_id":"owned-cluster","ca_fingerprint":"expected-pin"}`), nil
				case "invite":
					return []byte(`{"cluster_id":"owned-cluster","token":"owned-invitation"}`), nil
				case "prepare", "join", "serve-joined":
					return []byte(`{"completed":true}`), nil
				case "service":
					services++
					return []byte(`{"completed":true}`), nil
				case "bootstrap":
					if services != 3 || formal != 0 {
						t.Fatal("bootstrap did not follow all services and precede formal validation")
					}
					bootstraps++
					if fault == "unsupported" {
						return nil, errors.New("unsupported node action")
					}
					if fault == "malformed" {
						return []byte(`{"bootstrap_ready":"true"}`), nil
					}
					// These facts represent explicit source-pointer and installed
					// consumer prerequisites, rather than generic action success.
					ready := fault != "missing-pointer" && fault != "consumer-incomplete"
					return json.Marshal(map[string]bool{"bootstrap_ready": ready})
				case "status", "business", "read-setting":
					formal++
					if bootstraps != 3 {
						t.Error("formal business entered before all three material prerequisites")
					}
					if req.Action == "status" {
						return []byte(`{"status":"Status report\nOverall: ready (business reads verified)\n"}`), nil
					}
					return []byte(`{"completed":true}`), nil
				default:
					t.Fatalf("unknown literal node action: %s", req.Action)
					return nil, errors.New("unknown node action")
				}
			}
			var output bytes.Buffer
			err := installCluster(t.Context(), o, &output, remote, func(context.Context, string, string, io.Reader) error { return nil })
			if fault == "" {
				if err != nil || bootstraps != 3 || formal != 9 {
					t.Fatalf("bootstrap orchestration: err=%v checks=%d formal=%d", err, bootstraps, formal)
				}
			} else if err == nil || bootstraps != 1 || formal != 0 || strings.Contains(output.String(), "passed status") {
				t.Fatalf("failed bootstrap reached formal success: err=%v checks=%d formal=%d", err, bootstraps, formal)
			}
		})
	}
}
