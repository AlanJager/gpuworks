//go:build linux && dqlite

package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCandidateConsumerFailureRecordsClassWithoutChangingResult(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "consumer.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	receiver := &materialReceiver{root: filepath.Join(t.TempDir(), "material-replicas")}
	digest := strings.Repeat("a", 64)
	if err := st.DB().Create(&store.InferenceModel{ModelID: "private-model", Source: "upstream", PackageDigest: digest}).Error; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		canceled bool
		manifest materialManifest
		class    string
	}{
		{name: "missing_current_reference", class: "other"},
		{name: "canceled_query", canceled: true, class: "canceled"},
		{name: "current_reference", manifest: materialManifest{Files: []materialManifestFile{{Class: MaterialCatalogPackage, Path: "catalog-packages/" + digest + "/manifest.json"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			recorder := &candidateProofRecorder{}
			ctx := context.WithValue(base, candidateProofMetricsKey{}, recorder)
			wantErr := checkMaterialConsumerReferences(base, st, receiver, tc.manifest)
			gotErr := checkCandidateMaterialConsumerReferences(ctx, st, receiver, tc.manifest)
			if (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
				t.Fatalf("consumer result changed: got=%v want=%v", gotErr, wantErr)
			}
			metrics := recorder.snapshot()
			wantStage := ""
			if tc.class != "" {
				wantStage = "consumer_references"
			}
			if metrics.FailureStage != wantStage || metrics.FailureClass != tc.class {
				t.Fatalf("consumer failure diagnostic=%+v", metrics)
			}
			encoded, err := json.Marshal(metrics)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{receiver.root, digest, "private-model", "absent from", "context canceled"} {
				if strings.Contains(string(encoded), private) {
					t.Fatalf("metrics exposed consumer error data: %s", encoded)
				}
			}
		})
	}
}
