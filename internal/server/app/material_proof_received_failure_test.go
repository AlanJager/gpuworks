//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCandidateReceivedFailurePreservesVerifierResult(t *testing.T) {
	for _, name := range []string{"success", "missing_manifest_index", "redirected_file_index", "damaged_file", "missing_installed_master", "canceled_query"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			st, err := store.Open(filepath.Join(root, "proof.db"), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			body := []byte("private master material")
			master := filepath.Join(root, "master.key")
			if err := os.WriteFile(master, body, 0600); err != nil {
				t.Fatal(err)
			}
			replicaRoot := filepath.Join(root, "material-replicas")
			if err := os.Mkdir(replicaRoot, 0700); err != nil {
				t.Fatal(err)
			}
			receiver, err := newMaterialReceiver(replicaRoot, 7)
			if err != nil {
				t.Fatal(err)
			}
			expected := store.ClusterIdentity{ClusterID: strings.Repeat("a", 32)}
			key := strings.Repeat("b", 64)
			digest := materialTestDigest(body)
			raw, err := buildMaterialManifest(root, MaterialInventory{Classes: slices.Clone(requiredMaterialClasses), Files: []MaterialFile{{Class: MaterialMasterSecret, Path: master, SHA256: digest}}}, map[string]string{digest: master}, expected, 11, key)
			if err != nil {
				t.Fatal(err)
			}
			for _, data := range [][]byte{body, raw} {
				sum := materialTestDigest(data)
				if _, err := receiver.receive(t.Context(), sum, bytes.NewReader(data)); err != nil {
					t.Fatal(err)
				}
				if err := st.RecordLocalMaterialReplicaReference(t.Context(), sum, receiver.memberID, filepath.Join(replicaRoot, sum[:2], sum)); err != nil {
					t.Fatal(err)
				}
			}
			pointer := store.MaterialManifestRecord{SourceMemberID: 11, ClusterID: expected.ClusterID, SourceKey: key, Digest: materialTestDigest(raw), Bytes: int64(len(raw))}
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "missing_manifest_index":
				err = st.DB().Where("digest = ? AND member_id = ?", pointer.Digest, 7).Delete(&store.MaterialReplica{}).Error
			case "redirected_file_index":
				err = st.DB().Model(&store.MaterialReplica{}).Where("digest = ? AND member_id = ?", digest, 7).Update("path", master).Error
			case "damaged_file":
				err = os.WriteFile(filepath.Join(replicaRoot, digest[:2], digest), bytes.Repeat([]byte("x"), len(body)), 0600)
			case "missing_installed_master":
				err = os.Remove(master)
			case "canceled_query":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			recorder := &candidateProofRecorder{}
			ctx := context.WithValue(base, candidateProofMetricsKey{}, recorder)
			want, wantErr := verifyReceivedMaterialManifestBytes(base, st, receiver, expected, pointer)
			got, gotErr := verifyCandidateReceivedMaterialManifestBytes(ctx, st, receiver, expected, pointer)
			if !reflect.DeepEqual(got, want) || (gotErr == nil) != (wantErr == nil) || (gotErr != nil && gotErr.Error() != wantErr.Error()) {
				t.Fatalf("verifier result changed: got=%+v/%v want=%+v/%v", got, gotErr, want, wantErr)
			}
			metrics := recorder.snapshot()
			stage, class := "received_manifest", "other"
			if name == "success" {
				stage, class = "", ""
				if len(got.Files) != 1 {
					t.Fatal("successful verifier lost manifest")
				}
			} else if gotErr == nil {
				t.Fatal("real receiver failure was accepted")
			}
			if name == "canceled_query" {
				class = "canceled"
			}
			if metrics.FailureStage != stage || metrics.FailureClass != class {
				t.Fatalf("failure diagnostic=%+v want=%s/%s", metrics, stage, class)
			}
			if gotErr != nil {
				// A later real consumer failure must retain the first verifier failure.
				if err := st.DB().Create(&store.InferenceModel{ModelID: "private-model", Source: "upstream", PackageDigest: digest}).Error; err != nil {
					t.Fatal(err)
				}
				if err := checkCandidateMaterialConsumerReferences(context.WithValue(t.Context(), candidateProofMetricsKey{}, recorder), st, receiver, materialManifest{}); err == nil {
					t.Fatal("expected real consumer reference failure")
				}
				if after := recorder.snapshot(); after.FailureStage != stage || after.FailureClass != class {
					t.Fatalf("later failure replaced first: %+v", after)
				}
			}
			encoded, err := json.Marshal(recorder.snapshot())
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{root, digest, pointer.Digest, key, "private master material", "private-model", "context canceled", "missing or differs"} {
				if strings.Contains(string(encoded), private) {
					t.Fatalf("diagnostic leaked private data: %s", encoded)
				}
			}
		})
	}
}

func TestCandidateProofFailureRecorderIgnoresNilAndAbsentRecorder(t *testing.T) {
	recordCandidateProofFailure(t.Context(), "received_manifest", context.Canceled)
	var absent *candidateProofRecorder
	recordCandidateProofFailure(context.WithValue(t.Context(), candidateProofMetricsKey{}, absent), "received_manifest", context.Canceled)
	recorder := &candidateProofRecorder{}
	ctx := context.WithValue(t.Context(), candidateProofMetricsKey{}, recorder)
	recordCandidateProofFailure(ctx, "received_manifest", nil)
	if got := recorder.snapshot(); got != (CandidateProofMetrics{}) {
		t.Fatalf("nil failure changed metrics: %+v", got)
	}
	recordCandidateProofFailure(ctx, "received_manifest", context.Canceled)
	before := recorder.snapshot()
	recordCandidateProofFailure(ctx, "remote_quorum", nil)
	recordCandidateProofFailure(ctx, "remote_quorum", context.DeadlineExceeded)
	if got := recorder.snapshot(); got != before || got.FailureStage != "received_manifest" || got.FailureClass != "canceled" {
		t.Fatalf("first failure changed: %+v", got)
	}
}
